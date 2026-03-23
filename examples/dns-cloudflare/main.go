// Example: wildcard TLS via Cloudflare DNS-01
//
// Shows how to configure the tunnel server with a wildcard certificate
// obtained from Let's Encrypt using DNS-01 challenge via Cloudflare.
//
// Prerequisites:
//   - Domain managed by Cloudflare (e.g. estellarx.com)
//   - Wildcard DNS A record already in place: *.tunnel.estellarx.com → <server-ip>
//   - Cloudflare API Token with "Zone / DNS / Edit" permission:
//     https://dash.cloudflare.com/profile/api-tokens
//
// Environment variables:
//
//	CF_API_TOKEN   Cloudflare API token (scoped to DNS edit)
//
// Run the real server with (add --dns-provider flag once implemented in CLI):
//
//	tunnel-server serve \
//	  --tls-domain    tunnel.estellarx.com \
//	  --token         <auth-token>
//
// This file demonstrates the provider API directly so you can test
// credentials and DNS propagation before starting the full server.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/thiagozs/go-estellarx-tunnel/providers/cloudflare"
)

func main() {
	apiToken := mustEnv("CF_API_TOKEN")

	// The domain whose _acme-challenge record will be created.
	// Must be inside a zone managed by the Cloudflare account above.
	domain := envOr("TUNNEL_DOMAIN", "tunnel.estellarx.com")

	// ── Create the provider ───────────────────────────────────────────────────
	dns := cloudflare.New(apiToken)

	ctx := context.Background()

	// ── Verify credentials: create then delete a test TXT record ─────────────
	log.Printf("cloudflare: creating _acme-challenge TXT record for %s...", domain)

	recordID, err := dns.SetTXTRecord(ctx, domain, "estellarx-test-value")
	if err != nil {
		log.Fatalf("SetTXTRecord: %v", err)
	}
	log.Printf("cloudflare: created record id=%s", recordID)

	log.Printf("cloudflare: deleting test record id=%s...", recordID)
	if err := dns.DeleteTXTRecord(ctx, domain, recordID); err != nil {
		log.Fatalf("DeleteTXTRecord: %v", err)
	}
	log.Println("cloudflare: credentials OK — DNS-01 challenge will work")

	// ── How to wire this into the server ─────────────────────────────────────
	//
	//   dns := cloudflare.New(os.Getenv("CF_API_TOKEN"))
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
	fmt.Println("Done. Wire cloudflare.New() into newWildcardCertManager to use in the server.")
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
