package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	gomail "github.com/wneessen/go-mail"

	"badminton/internal/config"
	"badminton/internal/model"
)

// Sender delivers one message. The interface exists so tests can run the whole
// queue without an SMTP server.
type Sender interface {
	Send(ctx context.Context, m model.OutboxMessage) error
}

// ErrNotConfigured is returned when SMTP settings are missing. Messages fail
// loudly rather than being silently dropped, so the outbox shows the truth.
var ErrNotConfigured = errors.New("SMTP is not configured (set SMTP_HOST and SMTP_FROM)")

// SMTPSender delivers over SMTP.
type SMTPSender struct {
	cfg    config.SMTPConfig
	client *gomail.Client
}

// NewSMTPSender validates the SMTP settings and builds a reusable client.
// Configuration errors surface at boot rather than on the first send.
func NewSMTPSender(cfg config.SMTPConfig) (*SMTPSender, error) {
	if !cfg.Enabled() {
		return nil, ErrNotConfigured
	}
	opts := []gomail.Option{
		gomail.WithPort(cfg.Port),
		gomail.WithTimeout(30 * time.Second),
	}
	switch cfg.TLS {
	case "tls":
		opts = append(opts, gomail.WithSSL())
	case "none":
		opts = append(opts, gomail.WithTLSPolicy(gomail.NoTLS))
	default:
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	}
	if cfg.Username != "" {
		opts = append(opts,
			gomail.WithSMTPAuth(gomail.SMTPAuthAutoDiscover),
			gomail.WithUsername(cfg.Username),
			gomail.WithPassword(cfg.Password))
	} else {
		opts = append(opts, gomail.WithSMTPAuth(gomail.SMTPAuthNoAuth))
	}

	client, err := gomail.NewClient(cfg.Host, opts...)
	if err != nil {
		return nil, fmt.Errorf("smtp client: %w", err)
	}
	return &SMTPSender{cfg: cfg, client: client}, nil
}

// Send delivers one message as multipart/alternative.
func (s *SMTPSender) Send(ctx context.Context, m model.OutboxMessage) error {
	msg := gomail.NewMsg()
	if err := msg.FromFormat(s.cfg.FromName, s.cfg.From); err != nil {
		return fmt.Errorf("from address: %w", err)
	}
	if err := msg.AddToFormat(m.ToName, m.ToEmail); err != nil {
		// A malformed address is permanent, so say so rather than retrying it four more times.
		return fmt.Errorf("%w: %v", errPermanent, err)
	}
	// A reply should reach the organizer, not whatever account the app
	// authenticates as. People do reply to these -- "can't make it this week" --
	// and a thread a human can answer is the point.
	if m.ReplyTo != "" {
		if err := msg.ReplyTo(m.ReplyTo); err != nil {
			return fmt.Errorf("reply-to address: %w", err)
		}
	}
	msg.Subject(m.Subject)
	msg.SetBodyString(gomail.TypeTextPlain, m.TextBody)
	msg.AddAlternativeString(gomail.TypeTextHTML, m.HTMLBody)

	// No List-Unsubscribe header. It is one of the strongest signals a mail
	// client uses to decide a message is bulk, and it was helping put the
	// weekly invitation in Gmail's Promotions tab, where the people who need to
	// see it do not look. Every message still carries a visible unsubscribe
	// link in its footer, and this list is ~30 known club members rather than a
	// mailing at a scale where the header is expected of a sender. Restore the
	// header here if the volume ever grows to where that changes.

	return s.client.DialAndSendWithContext(ctx, msg)
}

// errPermanent marks failures that will never succeed, so the worker stops
// retrying them.
var errPermanent = errors.New("permanent failure")

// IsPermanent reports whether an error should not be retried.
func IsPermanent(err error) bool { return errors.Is(err, errPermanent) }

// DisabledSender fails every message with a clear reason. It is used when SMTP
// is unconfigured so the delivery log says why nothing went out, rather than
// pretending the mail was sent.
type DisabledSender struct{ Reason error }

// Send always fails with the configured reason.
func (d DisabledSender) Send(context.Context, model.OutboxMessage) error {
	if d.Reason != nil {
		return fmt.Errorf("%w: %v", errPermanent, d.Reason)
	}
	return fmt.Errorf("%w: %v", errPermanent, ErrNotConfigured)
}

// LogSender writes messages to the log instead of sending them. Useful when
// running locally without a mail catcher.
type LogSender struct{ Log *slog.Logger }

// Send logs the message and reports success.
func (l LogSender) Send(_ context.Context, m model.OutboxMessage) error {
	l.Log.Info("email (log sender)", "to", m.ToEmail, "subject", m.Subject, "kind", m.Kind)
	return nil
}
