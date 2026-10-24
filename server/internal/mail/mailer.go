// Package mail sends Blurt's transactional email. Only login PINs today.
package mail

import (
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"blurt/internal/config"
)

// Mailer hands login PINs to an SMTP server. smtp.SendMail owns the whole
// conversation: STARTTLS when the server offers it, certificate verification
// against the system roots, AUTH, and QUIT. PlainAuth refuses to send the
// password over a plaintext link, so there is no TLS policy of our own.
type Mailer struct {
	addr string
	from string
	auth smtp.Auth
}

// New builds the SMTP sender. SMTP settings are optional in the environment,
// so this is the single gate that refuses a half-configured mailbox: a real
// deploy with no host, port, or From address fails at boot.
func New(cfg config.SMTP) (*Mailer, error) {
	if cfg.Host == "" || cfg.From == "" || cfg.Port == 0 {
		return nil, fmt.Errorf("smtp host, port and from are required")
	}

	// No username means the relay takes mail without AUTH.
	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	}

	return &Mailer{
		addr: net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		from: cfg.From,
		auth: auth,
	}, nil
}

// SendPIN delivers the short-lived login code. The standard library dials,
// negotiates TLS, authenticates, and quits; the trade-off is that it brings no
// dial deadline of its own.
func (m *Mailer) SendPIN(to, pin string) error {
	return smtp.SendMail(m.addr, m.auth, m.from, []string{to}, pinMessage(m.from, to, pin))
}

// pinMessage is the RFC 5322 message that goes on the wire.
func pinMessage(from, to, pin string) []byte {
	body := fmt.Sprintf(
		"Your Blurt login PIN is %s.\r\n\r\nIt expires in 5 minutes and works once. "+
			"If you did not ask to sign in, ignore this email.\r\n", pin)
	return []byte(strings.Join([]string{
		"From: Blurt <" + from + ">",
		"To: " + to,
		"Subject: Your Blurt login PIN",
		"Date: " + time.Now().Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		body,
	}, "\r\n"))
}
