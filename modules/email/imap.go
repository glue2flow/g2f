package email

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // decode non-UTF-8 messages
	"github.com/emersion/go-message/mail"

	"github.com/glue2flow/g2f/internal/nodes"
)

const (
	maxReadLimit       = 200
	maxAttachmentBytes = 10 << 20 // per message, when content is included
	maxBodyBytes       = 2 << 20  // per text/html body
)

// connectIMAP logs in. The connection is terminated when ctx ends.
func connectIMAP(ctx context.Context, srv server) (*client.Client, error) {
	conn, err := dial(ctx, srv)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", srv.addr(), err)
	}
	conn.SetDeadline(time.Now().Add(commandTimeout))
	c, err := client.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("imap %s: %w", srv.addr(), err)
	}
	conn.SetDeadline(time.Time{})
	c.Timeout = commandTimeout
	fail := func(err error) (*client.Client, error) {
		c.Terminate()
		return nil, err
	}
	if srv.security == "starttls" {
		if ok, _ := c.SupportStartTLS(); !ok {
			return fail(fmt.Errorf("%s does not offer STARTTLS", srv.addr()))
		}
		if err := c.StartTLS(srv.tlsConfig()); err != nil {
			return fail(fmt.Errorf("imap starttls: %w", err))
		}
	}
	if srv.username == "" {
		return fail(errors.New("IMAP needs a username: set it in the module settings or on the node"))
	}
	if err := c.Login(srv.username, srv.password); err != nil {
		return fail(fmt.Errorf("imap login as %s failed: %w", srv.username, err))
	}
	return c, nil
}

func closeIMAP(c *client.Client) {
	if err := c.Logout(); err != nil {
		c.Terminate()
	}
}

// read searches a mailbox once per input item and emits the messages found.
func read(ctx context.Context, ec *nodes.ExecContext) (nodes.Output, error) {
	if len(ec.Items) == 0 {
		return nodes.Output{"main": {}}, nil
	}
	srv, err := settings(ec, "imap", 993, "tls")
	if err != nil {
		return nil, err
	}
	c, err := connectIMAP(ctx, srv)
	if err != nil {
		return nil, ctxErr(ctx, err)
	}
	defer closeIMAP(c)

	out := []nodes.Item{}
	for i, item := range ec.Items {
		res, err := readOnce(ctx, ec, c, item, i)
		if err != nil {
			return nil, ctxErr(ctx, err)
		}
		out = append(out, res...)
	}
	return nodes.Output{"main": out}, nil
}

type readOptions struct {
	mailbox            string
	criteria           *imap.SearchCriteria
	limit              int
	markSeen           bool
	includeAttachments bool
}

func readParams(ec *nodes.ExecContext, item nodes.Item, i int) (*readOptions, error) {
	o := &readOptions{criteria: imap.NewSearchCriteria(), limit: 20}
	var err error
	if o.mailbox, err = param(ec, "mailbox", item, i); err != nil {
		return nil, err
	}
	if o.mailbox == "" {
		o.mailbox = "INBOX"
	}
	if unseen, err := ec.Bool("unseenOnly", item, i); err != nil {
		return nil, err
	} else if unseen {
		o.criteria.WithoutFlags = []string{imap.SeenFlag}
	}
	since, err := param(ec, "since", item, i)
	if err != nil {
		return nil, err
	}
	if since != "" {
		t, err := parseDate(since)
		if err != nil {
			return nil, err
		}
		o.criteria.Since = t
	}
	for name, header := range map[string]string{"fromFilter": "From", "subjectFilter": "Subject"} {
		v, err := param(ec, name, item, i)
		if err != nil {
			return nil, err
		}
		if v != "" {
			if o.criteria.Header == nil {
				o.criteria.Header = textproto.MIMEHeader{}
			}
			o.criteria.Header.Add(header, v)
		}
	}
	if v, err := param(ec, "textFilter", item, i); err != nil {
		return nil, err
	} else if v != "" {
		o.criteria.Text = []string{v}
	}
	if n, err := ec.Number("limit", item, i); err != nil {
		return nil, err
	} else if n >= 1 {
		o.limit = min(int(n), maxReadLimit)
	}
	if o.markSeen, err = ec.Bool("markSeen", item, i); err != nil {
		return nil, err
	}
	if o.includeAttachments, err = ec.Bool("includeAttachments", item, i); err != nil {
		return nil, err
	}
	return o, nil
}

func parseDate(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("'Received since' must be a date like 2026-01-31, got %q", s)
}

func readOnce(ctx context.Context, ec *nodes.ExecContext, c *client.Client, item nodes.Item, i int) ([]nodes.Item, error) {
	o, err := readParams(ec, item, i)
	if err != nil {
		return nil, err
	}
	if _, err := c.Select(o.mailbox, !o.markSeen); err != nil {
		return nil, fmt.Errorf("select mailbox %q: %w", o.mailbox, err)
	}
	uids, err := c.UidSearch(o.criteria)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if len(uids) == 0 {
		return nil, nil
	}
	sort.Slice(uids, func(a, b int) bool { return uids[a] < uids[b] })
	if len(uids) > o.limit {
		uids = uids[len(uids)-o.limit:]
	}
	out, err := fetchMessages(c, uids, o)
	if err != nil {
		return nil, err
	}
	ec.Log(nodes.LogInfo, fmt.Sprintf("%s: %d message(s)", o.mailbox, len(out)))
	return out, nil
}

// fetchMessages downloads and parses the messages with the given UIDs of the
// selected mailbox, oldest first, and marks them as read if requested.
func fetchMessages(c *client.Client, uids []uint32, o *readOptions) ([]nodes.Item, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	set := new(imap.SeqSet)
	set.AddNum(uids...)

	section := &imap.BodySectionName{Peek: true}
	fetch := []imap.FetchItem{imap.FetchUid, imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, section.FetchItem()}
	ch := make(chan *imap.Message, 8)
	done := make(chan error, 1)
	go func() { done <- c.UidFetch(set, fetch, ch) }()
	var out []nodes.Item
	for msg := range ch {
		it := parseMessage(msg, section, o.includeAttachments)
		it["mailbox"] = o.mailbox
		if o.markSeen && !hasFlag(msg.Flags, imap.SeenFlag) {
			it["flags"] = append(it["flags"].([]any), imap.SeenFlag)
			it["seen"] = true
		}
		out = append(out, it)
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	sort.Slice(out, func(a, b int) bool { return out[a]["uid"].(uint32) < out[b]["uid"].(uint32) })
	for _, it := range out {
		it["uid"] = int(it["uid"].(uint32))
	}
	if o.markSeen {
		if err := c.UidStore(set, imap.FormatFlagsOp(imap.AddFlags, true), []any{imap.SeenFlag}, nil); err != nil {
			return nil, fmt.Errorf("mark as read: %w", err)
		}
	}
	return out, nil
}

func hasFlag(flags []string, f string) bool {
	for _, x := range flags {
		if strings.EqualFold(x, f) {
			return true
		}
	}
	return false
}

// parseMessage converts a fetched message into a workflow item. Parsing is
// lenient: a malformed message still yields its envelope data and an error note.
func parseMessage(msg *imap.Message, section *imap.BodySectionName, withContent bool) nodes.Item {
	flags := make([]any, len(msg.Flags))
	for i, f := range msg.Flags {
		flags[i] = f
	}
	it := nodes.Item{
		"uid": msg.Uid, "flags": flags, "seen": hasFlag(msg.Flags, imap.SeenFlag), "size": int(msg.Size),
		"to": []any{}, "cc": []any{}, "replyTo": []any{}, "references": []any{}, "attachments": []any{},
		"subject": "", "from": "", "fromName": "", "text": "", "html": "", "messageId": "", "inReplyTo": "",
	}
	if !msg.InternalDate.IsZero() {
		it["date"] = msg.InternalDate.UTC().Format(time.RFC3339)
	}
	body := msg.GetBody(section)
	if body == nil {
		it["parseError"] = "server returned no message body"
		return it
	}
	mr, err := mail.CreateReader(body)
	if err != nil && !message.IsUnknownCharset(err) {
		it["parseError"] = err.Error()
		return it
	}
	h := mr.Header
	if s, err := h.Subject(); err == nil {
		it["subject"] = s
	} else {
		it["subject"] = h.Get("Subject")
	}
	if from, err := h.AddressList("From"); err == nil && len(from) > 0 {
		it["from"], it["fromName"] = from[0].Address, from[0].Name
	}
	for key, field := range map[string]string{"to": "To", "cc": "Cc", "replyTo": "Reply-To"} {
		if list, err := h.AddressList(field); err == nil {
			addrs := make([]any, len(list))
			for i, a := range list {
				addrs[i] = a.Address
			}
			it[key] = addrs
		}
	}
	if d, err := h.Date(); err == nil && !d.IsZero() {
		it["date"] = d.UTC().Format(time.RFC3339)
	}
	if id, err := h.MessageID(); err == nil && id != "" {
		it["messageId"] = "<" + id + ">"
	}
	if ids, err := h.MsgIDList("In-Reply-To"); err == nil && len(ids) > 0 {
		it["inReplyTo"] = "<" + ids[0] + ">"
	}
	if ids, err := h.MsgIDList("References"); err == nil {
		refs := make([]any, len(ids))
		for i, id := range ids {
			refs[i] = "<" + id + ">"
		}
		it["references"] = refs
	}

	var text, html strings.Builder
	atts := []any{}
	budget := maxAttachmentBytes
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if message.IsUnknownCharset(err) && p != nil {
				// fall through with the undecoded body
			} else {
				it["parseError"] = err.Error()
				break
			}
		}
		switch ph := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := ph.ContentType()
			disp, _, _ := ph.ContentDisposition()
			switch {
			case ct == "text/plain" && disp != "attachment":
				b, _ := io.ReadAll(io.LimitReader(p.Body, maxBodyBytes))
				text.Write(b)
			case ct == "text/html" && disp != "attachment":
				b, _ := io.ReadAll(io.LimitReader(p.Body, maxBodyBytes))
				html.Write(b)
			default: // inline images and the like
				atts = append(atts, attachment(p.Body, filenameOf(ph.Header), ct, true, withContent, &budget))
			}
		case *mail.AttachmentHeader:
			ct, _, _ := ph.ContentType()
			name, _ := ph.Filename()
			atts = append(atts, attachment(p.Body, name, ct, false, withContent, &budget))
		}
	}
	it["text"], it["html"], it["attachments"] = text.String(), html.String(), atts
	return it
}

func filenameOf(h message.Header) string {
	if _, params, err := h.ContentDisposition(); err == nil && params["filename"] != "" {
		return params["filename"]
	}
	if _, params, err := h.ContentType(); err == nil {
		return params["name"]
	}
	return ""
}

func attachment(r io.Reader, name, ct string, inline, withContent bool, budget *int) nodes.Item {
	a := nodes.Item{"filename": name, "contentType": ct, "inline": inline}
	if withContent && *budget > 0 {
		var b bytes.Buffer
		n, _ := io.Copy(&b, io.LimitReader(r, int64(*budget)+1))
		if int(n) > *budget {
			a["truncated"] = true
			a["size"] = int(n) + countRest(r)
			*budget = 0
			return a
		}
		*budget -= int(n)
		a["size"] = int(n)
		a["content"] = base64.StdEncoding.EncodeToString(b.Bytes())
		return a
	}
	a["size"] = countRest(r)
	return a
}

func countRest(r io.Reader) int {
	n, _ := io.Copy(io.Discard, r)
	return int(n)
}

// --- move / flags -----------------------------------------------------------

type uidTarget struct {
	item    nodes.Item
	mailbox string
	uid     uint32
}

func uidTargets(ec *nodes.ExecContext) ([]uidTarget, error) {
	out := make([]uidTarget, 0, len(ec.Items))
	for i, item := range ec.Items {
		uid, err := uidParam(ec, item, i)
		if err != nil {
			return nil, err
		}
		mb, err := param(ec, "mailbox", item, i)
		if err != nil {
			return nil, err
		}
		if mb == "" {
			mb = "INBOX"
		}
		out = append(out, uidTarget{item: item, mailbox: mb, uid: uid})
	}
	return out, nil
}

// withSelected runs fn for each target with its mailbox selected read-write.
func withSelected(ctx context.Context, ec *nodes.ExecContext, fn func(c *client.Client, t uidTarget, i int) (nodes.Item, error)) (nodes.Output, error) {
	if len(ec.Items) == 0 {
		return nodes.Output{"main": {}}, nil
	}
	targets, err := uidTargets(ec)
	if err != nil {
		return nil, err
	}
	srv, err := settings(ec, "imap", 993, "tls")
	if err != nil {
		return nil, err
	}
	c, err := connectIMAP(ctx, srv)
	if err != nil {
		return nil, ctxErr(ctx, err)
	}
	defer closeIMAP(c)
	selected := ""
	out := make([]nodes.Item, 0, len(targets))
	for i, t := range targets {
		if t.mailbox != selected {
			if _, err := c.Select(t.mailbox, false); err != nil {
				return nil, ctxErr(ctx, fmt.Errorf("select mailbox %q: %w", t.mailbox, err))
			}
			selected = t.mailbox
		}
		res, err := fn(c, t, i)
		if err != nil {
			return nil, ctxErr(ctx, fmt.Errorf("item %d (UID %d): %w", i, t.uid, err))
		}
		out = append(out, res)
	}
	return nodes.Output{"main": out}, nil
}

func move(ctx context.Context, ec *nodes.ExecContext) (nodes.Output, error) {
	return withSelected(ctx, ec, func(c *client.Client, t uidTarget, i int) (nodes.Item, error) {
		target, err := required(ec, "target", t.item, i)
		if err != nil {
			return nil, err
		}
		if err := moveUID(ec, c, t.uid, target); err != nil {
			return nil, err
		}
		res := copyItem(t.item)
		res["movedTo"] = target
		return res, nil
	})
}

// moveUID moves one message. Without a working MOVE command it copies the
// message, flags the original \Deleted and removes it with UID EXPUNGE
// (UIDPLUS). A plain EXPUNGE is never used: it would also purge other
// messages someone flagged as deleted.
func moveUID(ec *nodes.ExecContext, c *client.Client, uid uint32, target string) error {
	set := new(imap.SeqSet)
	set.AddNum(uid)
	if ok, _ := c.Support("MOVE"); ok {
		err := c.UidMove(set, target)
		if err == nil || c.State() == imap.LogoutState {
			return err
		}
		// Some servers advertise MOVE but refuse it; fall back below.
	}
	if err := c.UidCopy(set, target); err != nil {
		return fmt.Errorf("copy to %q: %w", target, err)
	}
	if err := c.UidStore(set, imap.FormatFlagsOp(imap.AddFlags, true), []any{imap.DeletedFlag}, nil); err != nil {
		return fmt.Errorf("flag original as deleted: %w", err)
	}
	if ok, _ := c.Support("UIDPLUS"); !ok {
		ec.Log(nodes.LogWarn, fmt.Sprintf("UID %d copied to %s and flagged \\Deleted; the server cannot expunge single messages (no UIDPLUS)", uid, target))
		return nil
	}
	status, err := c.Execute(uidExpunge{set}, nil)
	if err == nil {
		err = status.Err()
	}
	if err != nil {
		return fmt.Errorf("expunge original: %w", err)
	}
	return nil
}

type uidExpunge struct{ set *imap.SeqSet }

func (u uidExpunge) Command() *imap.Command {
	return &imap.Command{Name: "UID", Arguments: []any{imap.RawString("EXPUNGE"), u.set}}
}

func setFlags(ctx context.Context, ec *nodes.ExecContext) (nodes.Output, error) {
	return withSelected(ctx, ec, func(c *client.Client, t uidTarget, i int) (nodes.Item, error) {
		action, err := param(ec, "action", t.item, i)
		if err != nil {
			return nil, err
		}
		op := imap.FlagsOp(imap.AddFlags)
		switch action {
		case "", "add":
		case "remove":
			op = imap.RemoveFlags
		default:
			return nil, fmt.Errorf("invalid action %q", action)
		}
		raw, err := param(ec, "flags", t.item, i)
		if err != nil {
			return nil, err
		}
		var flags []any
		for _, f := range nodes.ParseList(raw) {
			if strings.ContainsAny(f, " ()[]{\"\r\n") {
				return nil, fmt.Errorf("invalid flag %q", f)
			}
			flags = append(flags, imap.CanonicalFlag(f))
		}
		if len(flags) == 0 {
			return nil, errors.New("no flags given")
		}
		set := new(imap.SeqSet)
		set.AddNum(t.uid)
		if err := c.UidStore(set, imap.FormatFlagsOp(op, true), flags, nil); err != nil {
			return nil, err
		}
		return copyItem(t.item), nil
	})
}
