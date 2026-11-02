package app

import (
	"fmt"
	"log"
	"mime"
	"net/smtp"
	"strings"
	"time"
)

type SMTPConfig struct {
	Host, Port, User, Pass, From string
}

func (s *Server) mailConfigured() bool { return s.smtp.Host != "" }

// sendMail sends a plain-text email, or only logs it when SMTP is not configured.
func (s *Server) sendMail(to, subject, body string) error {
	if !s.mailConfigured() {
		log.Printf("SMTP not configured; email to %s (%s):\n%s", to, subject, body)
		return nil
	}
	// Subjects can contain user-chosen names: no line breaks (header injection), and
	// non-ASCII text must be encoded.
	subject = mime.QEncoding.Encode("utf-8", strings.NewReplacer("\r", " ", "\n", " ").Replace(subject))
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		s.smtp.From, to, subject, time.Now().Format(time.RFC1123Z), body)
	var auth smtp.Auth
	if s.smtp.User != "" {
		auth = smtp.PlainAuth("", s.smtp.User, s.smtp.Pass, s.smtp.Host)
	}
	return smtp.SendMail(s.smtp.Host+":"+s.smtp.Port, auth, s.smtp.From, []string{to}, []byte(msg))
}
