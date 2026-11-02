package main

import (
	"fmt"
	"log"
	"net/smtp"
	"time"
)

type smtpConfig struct {
	host, port, user, pass, from string
}

func (s *server) mailConfigured() bool { return s.smtp.host != "" }

// sendMail sends a plain-text email, or only logs it when SMTP is not configured.
func (s *server) sendMail(to, subject, body string) error {
	if !s.mailConfigured() {
		log.Printf("SMTP not configured; email to %s (%s):\n%s", to, subject, body)
		return nil
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		s.smtp.from, to, subject, time.Now().Format(time.RFC1123Z), body)
	var auth smtp.Auth
	if s.smtp.user != "" {
		auth = smtp.PlainAuth("", s.smtp.user, s.smtp.pass, s.smtp.host)
	}
	return smtp.SendMail(s.smtp.host+":"+s.smtp.port, auth, s.smtp.from, []string{to}, []byte(msg))
}
