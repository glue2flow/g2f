package email

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/emersion/go-imap"

	"github.com/glue2flow/g2f/internal/nodes"
)

// cursor is what the Email Received trigger remembers per mailbox: the last
// UID it has handled. UIDs only grow while UIDVALIDITY stays the same; when
// the server resets them, the trigger starts over from the current state.
type cursor struct {
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uidValidity"`
	LastUID     uint32 `json:"lastUid"`
}

// received is the check of the Email Received polling trigger.
func received(ctx context.Context, ec *nodes.ExecContext) (nodes.Output, error) {
	test := ec.IsTestRun()
	if !test && ec.State == nil {
		return nil, nodes.ErrNoState
	}
	srv, err := settings(ec, "imap", 993, "tls")
	if err != nil {
		return nil, err
	}
	o, err := readParams(ec, nil, 0)
	if err != nil {
		return nil, err
	}
	if test {
		o.markSeen = false // a test shows mail, it does not change the mailbox
	}
	c, err := connectIMAP(ctx, srv)
	if err != nil {
		return nil, ctxErr(ctx, err)
	}
	defer closeIMAP(c)
	mbox, err := c.Select(o.mailbox, !o.markSeen)
	if err != nil {
		return nil, ctxErr(ctx, fmt.Errorf("select mailbox %q: %w", o.mailbox, err))
	}

	if test {
		uids, err := c.UidSearch(o.criteria)
		if err != nil {
			return nil, ctxErr(ctx, fmt.Errorf("search: %w", err))
		}
		sort.Slice(uids, func(a, b int) bool { return uids[a] < uids[b] })
		if len(uids) > o.limit {
			uids = uids[len(uids)-o.limit:]
		}
		out, err := fetchMessages(c, uids, o)
		return nodes.Output{"main": out}, ctxErr(ctx, err)
	}

	scope := "imap:" + ec.Node.ID
	var cur cursor
	if raw, ok, err := ec.State.Get(ctx, scope, "cursor"); err != nil {
		return nil, err
	} else if ok {
		// A damaged cursor is replaced below, like a missing one.
		if b, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(b, &cur)
		}
	}
	uidNext := mbox.UidNext
	if uidNext == 0 { // not reported: derive it from the highest UID
		all, err := c.UidSearch(imap.NewSearchCriteria())
		if err != nil {
			return nil, ctxErr(ctx, err)
		}
		uidNext = 1
		for _, u := range all {
			uidNext = max(uidNext, u+1)
		}
	}
	save := func(last uint32) error {
		return ec.State.Set(ctx, scope, "cursor", cursor{Mailbox: o.mailbox, UIDValidity: mbox.UidValidity, LastUID: last}, 0)
	}
	if cur.Mailbox != o.mailbox || cur.UIDValidity != mbox.UidValidity || cur.UIDValidity == 0 {
		if err := save(uidNext - 1); err != nil {
			return nil, err
		}
		ec.Log(nodes.LogInfo, fmt.Sprintf("%s: started watching; existing mail is ignored", o.mailbox))
		return nodes.Output{"main": {}}, nil
	}

	crit := o.criteria
	crit.Uid = new(imap.SeqSet)
	crit.Uid.AddRange(cur.LastUID+1, 0) // last+1:*
	found, err := c.UidSearch(crit)
	if err != nil {
		return nil, ctxErr(ctx, fmt.Errorf("search: %w", err))
	}
	var uids []uint32
	for _, u := range found {
		if u > cur.LastUID { // "n:*" always matches the newest message
			uids = append(uids, u)
		}
	}
	sort.Slice(uids, func(a, b int) bool { return uids[a] < uids[b] })
	last := max(cur.LastUID, uidNext-1)
	if len(uids) > o.limit {
		uids = uids[:o.limit] // the rest follows with the next check
		last = uids[len(uids)-1]
	} else if len(uids) > 0 {
		last = max(last, uids[len(uids)-1])
	}
	out, err := fetchMessages(c, uids, o)
	if err != nil {
		return nil, ctxErr(ctx, err)
	}
	if last != cur.LastUID {
		if err := save(last); err != nil {
			return nil, err
		}
	}
	return nodes.Output{"main": out}, nil
}
