package email

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/emersion/go-imap/client"

	"github.com/glue2flow/g2f/internal/nodes"
)

// idleRestart: IDLE is renewed this often, below the 30 minutes after which
// servers may log out an idle client (RFC 2177). A connection that does not
// answer the renewal within commandTimeout is considered dead.
var idleRestart = 25 * time.Minute

// listen keeps an IMAP IDLE connection on the Email Received trigger's
// mailbox and wakes the trigger when the server reports new messages
// (EXISTS or RECENT). The check then finds them by UID as usual. Server and
// login come from the same settings as the check (organization settings or
// the node's credential).
func listen(ctx context.Context, ec *nodes.ExecContext, wake func(), connected func()) error {
	srv, err := settings(ec, "imap", 993, "tls")
	if err != nil {
		return err
	}
	o, err := readParams(ec, nil, 0)
	if err != nil {
		return err
	}
	c, err := connectIMAP(ctx, srv)
	if err != nil {
		return ctxErr(ctx, err)
	}
	defer func() {
		if ctx.Err() != nil {
			c.Terminate() // the connection is already closed
		} else {
			closeIMAP(c)
		}
	}()
	if ok, err := c.Support("IDLE"); err != nil {
		return ctxErr(ctx, err)
	} else if !ok {
		return fmt.Errorf("%s does not offer IDLE: %w", srv.host, nodes.ErrPushUnsupported)
	}
	updates := make(chan client.Update, 64)
	c.Updates = updates
	if _, err := c.Select(o.mailbox, true); err != nil {
		return ctxErr(ctx, fmt.Errorf("select mailbox %q: %w", o.mailbox, err))
	}
	// The counts reported by SELECT are not news: connected wakes anyway.
	for drained := false; !drained; {
		select {
		case <-updates:
		default:
			drained = true
		}
	}
	connected()
	// The deadline of each IDLE command covers the renewal.
	c.Timeout = idleRestart + commandTimeout
	for {
		stop := make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- c.Idle(stop, &client.IdleOptions{LogoutTimeout: -1}) }()
		renew := time.NewTimer(idleRestart)
		stopped := false
	round:
		for {
			select {
			case u := <-updates:
				if _, ok := u.(*client.MailboxUpdate); ok {
					wake()
				}
			case <-renew.C:
				close(stop)
				stopped = true
			case <-ctx.Done():
				renew.Stop()
				return ctx.Err()
			case err := <-done:
				renew.Stop()
				if err != nil {
					return ctxErr(ctx, fmt.Errorf("idle: %w", err))
				}
				if !stopped {
					return errors.New("the server ended IDLE")
				}
				break round
			}
		}
	}
}
