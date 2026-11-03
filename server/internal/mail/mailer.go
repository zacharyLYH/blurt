// Package mail sends Blurt's transactional email: login PINs, and the yes or no
// that answers an application.
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

// Mailer hands messages to an SMTP server. smtp.SendMail owns the whole
// conversation: STARTTLS when the server offers it, certificate verification
// against the system roots, AUTH, and QUIT. PlainAuth refuses to send the
// password over a plaintext link, so there is no TLS policy of our own.
type Mailer struct {
	addr string
	from string
	auth smtp.Auth
	// site is the origin the app is served from, so a mail can link back to it.
	site string
}

// New builds the SMTP sender. SMTP settings are optional in the environment,
// so this is the single gate that refuses a half-configured mailbox: a real
// deploy with no host, port, or From address fails at boot.
func New(cfg config.SMTP, site string) (*Mailer, error) {
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
		site: strings.TrimRight(site, "/"),
	}, nil
}

// SendPIN delivers the short-lived login code. The standard library dials,
// negotiates TLS, authenticates, and quits; the trade-off is that it brings no
// dial deadline of its own.
func (m *Mailer) SendPIN(to, pin string) error {
	body := fmt.Sprintf(
		"Your Blurt login PIN is %s.\r\n\r\nIt expires in 5 minutes and works once. "+
			"If you did not ask to sign in, ignore this email.\r\n", pin)
	return m.send(to, "Your Blurt login PIN", body)
}

// SendApproved tells an applicant they are in, with the door in the message.
func (m *Mailer) SendApproved(to, name string) error {
	body := fmt.Sprintf(
		"Hi %s,\r\n\r\nYour Blurt account is ready. Sign in with this email address and we will "+
			"mail you a PIN:\r\n\r\n%s/login\r\n\r\nBlurt turns what you say into reminders you "+
			"can trust.\r\n", name, m.site)
	return m.send(to, "You're in — Blurt", body)
}

// SendRejected closes the loop. The row is already gone by the time this sends,
// so a failure here costs the applicant an explanation, never their privacy.
func (m *Mailer) SendRejected(to, name string) error {
	body := fmt.Sprintf(
		"Hi %s,\r\n\r\nThanks for applying to Blurt. We are not opening an account for this "+
			"address right now.\r\n", name)
	return m.send(to, "Your Blurt application", body)
}

// send writes one plain-text RFC 5322 message.
func (m *Mailer) send(to, subject, body string) error {
	return smtp.SendMail(m.addr, m.auth, m.from, []string{to}, message(m.from, to, subject, body))
}

func message(from, to, subject, body string) []byte {
	return []byte(strings.Join([]string{
		"From: Blurt <" + from + ">",
		"To: " + to,
		"Subject: " + subject,
		"Date: " + time.Now().Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		body,
	}, "\r\n"))
}
