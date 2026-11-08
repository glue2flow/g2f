[Docs](README.md) › Email (SMTP/IMAP)

# Email (SMTP/IMAP)

The `email` module ([`modules/email`](../modules/email)) sends mail over SMTP and reads mail
over IMAP, with any mail server. It is a [native Go module](plugins.md#native-go-modules).
For Gmail and Microsoft 365 accounts, the [OAuth2 plugins](connections.md#included-oauth2-plugins)
are usually the better choice.

## Setup

An organization admin enters the servers in *Plugins → Email (SMTP/IMAP) → Settings*: host,
port, security (`tls`, `starttls` or `none`), username, password and the default sender.
Nodes can set their own **Username/Password** to use a personal mailbox on the same server.
Node fields cannot change the host.

Passwords are only sent over TLS/STARTTLS, or to `localhost`.

| Provider | SMTP | IMAP |
| --- | --- | --- |
| Gmail (app password) | `smtp.gmail.com` 587 starttls | `imap.gmail.com` 993 tls |
| Microsoft 365 (if basic auth is allowed) | `smtp.office365.com` 587 starttls | `outlook.office365.com` 993 tls |

## Nodes

| Node | What it does |
| --- | --- |
| **Send Email** | One mail per item: From, To/Cc/Bcc (comma separated or arrays), Reply-To, subject, text and/or HTML, attachments `[{filename, content (base64), contentType}]`, extra headers (e.g. `In-Reply-To`). Bcc stays out of the headers. |
| **Read Emails** | Searches a mailbox (unread only, since a date, From/Subject/text contains) and emits the newest N messages, oldest first, with `uid`, `from`, `to`, `subject`, `date`, `text`, `html`, `flags`, `messageId` and attachment info (optionally with content). *Mark as read* makes the next run skip them. |
| **Move Email** | Moves messages by `uid` to another mailbox (MOVE, or copy + UID EXPUNGE). A plain EXPUNGE is never used, so other deleted messages are not purged. |
| **Set Email Flags** | Adds or removes flags such as `\Seen` or `\Flagged`. |

```
Schedule (every 5 min) ─► Read Emails (unread, mark as read) ─► If subject contains "invoice"
   ─► HTTP Request (upload attachment) ─► Move Email (Archive)
```
