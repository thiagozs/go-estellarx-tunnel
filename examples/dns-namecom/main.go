// Example: wildcard TLS via name.com DNS-01
//
// Shows how to configure the tunnel server with a wildcard certificate
// obtained from Let's Encrypt using DNS-01 challenge via name.com.
//
// Prerequisites:
//   - Domain managed by name.com (e.g. estellarx.com)
//   - Wildcard DNS A record already in place: *.tunnel.estellarx.com → <server-ip>
//   - name.com API credentials: https://www.name.com/account/settings/api
//
// Environment variables:
//
//	NAMECOM_USER   name.com API username
//	NAMECOM_TOKEN  name.com API token
//
// Run the real server with:
//
//	tunnel-server serve \
//	  --tls-domain        tunnel.estellarx.com \
//	  --dns-namecom-user  $NAMECOM_USER \
//	  --dns-namecom-token $NAMECOM_TOKEN \
//	  --token             <auth-token>
//
// This file demonstrates the provider API directly so you can test
// credentials and DNS propagation before starting the full server.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/thiagozs/go-estellarx-tunnel/providers/namecom"
)

func main() {
	user := mustEnv("NAMECOM_USER")
	token := mustEnv("NAMECOM_TOKEN")

	// The domain whose _acme-challenge record will be created.
	// Must be managed by the name.com account above.
	domain := envOr("TUNNEL_DOMAIN", "tunnel.estellarx.com")

	// ── Create the provider ───────────────────────────────────────────────────
	dns := namecom.New(user, token)

	ctx := context.Background()

	// ── Verify credentials: create then delete a test TXT record ─────────────
	log.Printf("name.com: creating _acme-challenge TXT record for %s...", domain)

	recordID, err := dns.SetTXTRecord(ctx, domain, "estellarx-test-value")
	if err != nil {
		log.Fatalf("SetTXTRecord: %v", err)
	}
	log.Printf("name.com: created record id=%s", recordID)

	log.Printf("name.com: deleting test record id=%s...", recordID)
	if err := dns.DeleteTXTRecord(ctx, domain, recordID); err != nil {
		log.Fatalf("DeleteTXTRecord: %v", err)
	}
	log.Println("name.com: credentials OK — DNS-01 challenge will work")

	// ── How the server wires this up (server/main.go) ─────────────────────────
	//
	//   dns := namecom.New(namecomUser, namecomToken)
	//
	//   wcm := newWildcardCertManager(tlsDomain, certCacheDir, dns)
	//   if err := wcm.Start(ctx); err != nil {
	//       log.Fatal(err)
	//   }
	//   srv := &http.Server{
	//       Addr:      ":443",
	//       Handler:   mux,
	//       TLSConfig: wcm.TLSConfig(),
	//   }
	//   log.Fatal(srv.ListenAndServeTLS("", ""))
	fmt.Println("Done. Start the server with the flags shown in the file header.")
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
