// Command envgen writes .env from .env.example with the secrets that can be
// generated locally (session secret, VAPID pair). Human values — SMTP app
// password, agent key, public origin — are left for the operator.
package main

import (
	"bufio"
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

func main() {
	example := flag.String("example", "../.env.example", "template env file")
	out := flag.String("out", "../.env", "env file to write")
	force := flag.Bool("force", false, "overwrite an existing env file")
	flag.Parse()

	if _, err := os.Stat(*out); err == nil && !*force {
		fmt.Printf("%s already exists; leaving it untouched\n", *out)
		return
	}

	content, err := os.ReadFile(*example)
	if err != nil {
		log.Fatalf("read %s: %v", *example, err)
	}

	sessionSecret := make([]byte, 32)
	if _, err := rand.Read(sessionSecret); err != nil {
		log.Fatalf("generate secret: %v", err)
	}
	vapidKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		log.Fatalf("generate VAPID key: %v", err)
	}

	generated := map[string]string{
		"BLURT_SESSION_SECRET":    hex.EncodeToString(sessionSecret),
		"BLURT_VAPID_PUBLIC_KEY":  base64.RawURLEncoding.EncodeToString(vapidKey.PublicKey().Bytes()),
		"BLURT_VAPID_PRIVATE_KEY": base64.RawURLEncoding.EncodeToString(vapidKey.Bytes()),
	}

	var result strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		name, _, _ := strings.Cut(line, "=")
		if value, ok := generated[strings.TrimSpace(name)]; ok {
			line = name + "=" + value
		}
		result.WriteString(line + "\n")
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("read %s: %v", *example, err)
	}

	if err := os.WriteFile(*out, []byte(result.String()), 0o600); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	fmt.Printf("wrote %s with generated session secret and VAPID keys\n", *out)
	fmt.Println("still needed: BLURT_SMTP_PASSWORD (Gmail App Password), BLURT_AGENT_API_KEY/BLURT_AGENT_MODEL, and BLURT_PUBLIC_ORIGINS for a public host")
}
