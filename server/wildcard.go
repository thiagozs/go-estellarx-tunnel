package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/thiagozs/go-estellarx-tunnel/providers"
	"golang.org/x/crypto/acme"
)

// wildcardCertManager obtains and renews a wildcard TLS certificate for *.domain
// using Let's Encrypt DNS-01 challenge delegated to a providers.DNSProvider.
type wildcardCertManager struct {
	domain   string
	cacheDir string
	provider providers.DNSProvider

	mu   sync.RWMutex
	cert *tls.Certificate
}

func newWildcardCertManager(domain, cacheDir string, provider providers.DNSProvider) *wildcardCertManager {
	return &wildcardCertManager{
		domain:   domain,
		cacheDir: cacheDir,
		provider: provider,
	}
}

// Start loads or obtains the wildcard cert and launches the renewal goroutine.
// Blocks until the first certificate is ready.
func (w *wildcardCertManager) Start(ctx context.Context) error {
	if err := os.MkdirAll(w.cacheDir, 0700); err != nil {
		return fmt.Errorf("wildcard: mkdir %s: %w", w.cacheDir, err)
	}

	// Use cached cert if it is still valid for more than 30 days.
	if cert, err := w.loadCachedCert(); err == nil {
		remaining := time.Until(cert.Leaf.NotAfter)
		if remaining > 30*24*time.Hour {
			log.Printf("wildcard: loaded from cache (expires %s, %.0f days remaining)",
				cert.Leaf.NotAfter.Format("2006-01-02"), remaining.Hours()/24)
			w.mu.Lock()
			w.cert = cert
			w.mu.Unlock()
			go w.renewLoop(ctx)
			return nil
		}
		log.Printf("wildcard: cached cert expires in %.0f days — renewing now", remaining.Hours()/24)
	}

	if err := w.obtainAndSave(ctx); err != nil {
		return err
	}
	go w.renewLoop(ctx)
	return nil
}

// TLSConfig returns a *tls.Config that serves the wildcard certificate.
func (w *wildcardCertManager) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
			w.mu.RLock()
			defer w.mu.RUnlock()
			if w.cert == nil {
				return nil, fmt.Errorf("wildcard: certificate not yet available")
			}
			return w.cert, nil
		},
		NextProtos: []string{"h2", "http/1.1"},
	}
}

// renewLoop sleeps until 30 days before expiry, then renews.
func (w *wildcardCertManager) renewLoop(ctx context.Context) {
	for {
		w.mu.RLock()
		cert := w.cert
		w.mu.RUnlock()

		renewAt := cert.Leaf.NotAfter.Add(-30 * 24 * time.Hour)
		wait := max(time.Until(renewAt), 0)
		log.Printf("wildcard: next renewal scheduled in %s", wait.Round(time.Hour))

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		log.Printf("wildcard: renewing certificate...")
		if err := w.obtainAndSave(ctx); err != nil {
			log.Printf("wildcard: renewal failed: %v — retrying in 1h", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Hour):
			}
		}
	}
}

// obtainAndSave performs the full ACME DNS-01 flow and persists the result.
func (w *wildcardCertManager) obtainAndSave(ctx context.Context) error {
	domains := []string{w.domain, "*." + w.domain}

	accountKey, err := w.loadOrCreateAccountKey()
	if err != nil {
		return fmt.Errorf("wildcard: account key: %w", err)
	}

	client := &acme.Client{
		Key:          accountKey,
		DirectoryURL: acme.LetsEncryptURL,
	}

	if err := w.ensureAccount(ctx, client); err != nil {
		return fmt.Errorf("wildcard: account: %w", err)
	}

	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("wildcard: generate cert key: %w", err)
	}

	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(domains...))
	if err != nil {
		return fmt.Errorf("wildcard: authorize order: %w", err)
	}

	// Track records created so we can clean them up afterwards.
	type dnsCleanup struct {
		domain   string
		recordID string
	}
	var cleanups []dnsCleanup

	defer func() {
		for _, c := range cleanups {
			if err := w.provider.DeleteTXTRecord(ctx, c.domain, c.recordID); err != nil {
				log.Printf("wildcard: cleanup TXT record for %s: %v", c.domain, err)
			}
		}
	}()

	for _, authURL := range order.AuthzURLs {
		auth, err := client.GetAuthorization(ctx, authURL)
		if err != nil {
			return fmt.Errorf("wildcard: get authorization: %w", err)
		}
		if auth.Status == "valid" {
			continue
		}

		var chal *acme.Challenge
		for _, c := range auth.Challenges {
			if c.Type == "dns-01" {
				chal = c
				break
			}
		}
		if chal == nil {
			return fmt.Errorf("wildcard: no dns-01 challenge for %s", auth.Identifier.Value)
		}

		txtValue, err := client.DNS01ChallengeRecord(chal.Token)
		if err != nil {
			return fmt.Errorf("wildcard: dns01 record value: %w", err)
		}
		if txtValue == "" {
			return fmt.Errorf("wildcard: dns01 record value is empty for token %q", chal.Token)
		}
		log.Printf("wildcard: DNS-01 challenge for %s — TXT value=%s", auth.Identifier.Value, txtValue)

		recID, err := w.provider.SetTXTRecord(ctx, auth.Identifier.Value, txtValue)
		if err != nil {
			return fmt.Errorf("wildcard: set TXT for %s: %w", auth.Identifier.Value, err)
		}
		cleanups = append(cleanups, dnsCleanup{domain: auth.Identifier.Value, recordID: recID})

		// Give DNS time to propagate before signalling Let's Encrypt.
		log.Printf("wildcard: waiting 15s for DNS propagation...")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}

		if _, err := client.Accept(ctx, chal); err != nil {
			return fmt.Errorf("wildcard: accept challenge for %s: %w", auth.Identifier.Value, err)
		}
		if _, err := client.WaitAuthorization(ctx, authURL); err != nil {
			return fmt.Errorf("wildcard: wait authorization for %s: %w", auth.Identifier.Value, err)
		}
		log.Printf("wildcard: authorization valid for %s", auth.Identifier.Value)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: "*." + w.domain},
		DNSNames: domains,
	}, certKey)
	if err != nil {
		return fmt.Errorf("wildcard: create CSR: %w", err)
	}

	der, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return fmt.Errorf("wildcard: finalize order: %w", err)
	}

	if err := w.saveCert(certKey, der); err != nil {
		return fmt.Errorf("wildcard: save cert: %w", err)
	}

	cert, err := w.loadCachedCert()
	if err != nil {
		return fmt.Errorf("wildcard: load after save: %w", err)
	}

	w.mu.Lock()
	w.cert = cert
	w.mu.Unlock()

	log.Printf("wildcard: certificate obtained (expires %s)", cert.Leaf.NotAfter.Format("2006-01-02"))
	return nil
}

// ─── Key & cert persistence ───────────────────────────────────────────────────

func (w *wildcardCertManager) accountKeyPath() string {
	return filepath.Join(w.cacheDir, "acme-account.key")
}

func (w *wildcardCertManager) certPath() string { return filepath.Join(w.cacheDir, "wildcard.crt") }
func (w *wildcardCertManager) keyPath() string  { return filepath.Join(w.cacheDir, "wildcard.key") }

func (w *wildcardCertManager) loadOrCreateAccountKey() (crypto.Signer, error) {
	data, err := os.ReadFile(w.accountKeyPath())
	if err == nil {
		block, _ := pem.Decode(data)
		if block != nil {
			if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
				return key, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(w.accountKeyPath(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	_ = pem.Encode(f, &pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	return key, nil
}

func (w *wildcardCertManager) ensureAccount(ctx context.Context, client *acme.Client) error {
	if _, err := client.GetReg(ctx, ""); err == nil {
		return nil
	}
	if _, err := client.Register(ctx, &acme.Account{}, acme.AcceptTOS); err != nil {
		return err
	}
	return nil
}

func (w *wildcardCertManager) saveCert(key *ecdsa.PrivateKey, der [][]byte) error {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	kf, err := os.OpenFile(w.keyPath(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer kf.Close()
	if err := pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		return err
	}

	cf, err := os.OpenFile(w.certPath(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer cf.Close()
	for _, d := range der {
		if err := pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: d}); err != nil {
			return err
		}
	}
	return nil
}

func (w *wildcardCertManager) loadCachedCert() (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(w.certPath(), w.keyPath())
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	cert.Leaf = leaf
	return &cert, nil
}
