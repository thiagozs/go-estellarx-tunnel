// Package namecom implements providers.DNSProvider using the name.com REST API.
// Docs: https://www.name.com/api-docs
package namecom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

const apiBase = "https://api.name.com/v4"

// Provider satisfies providers.DNSProvider for name.com domains.
type Provider struct {
	user  string
	token string
}

// New returns a name.com DNS provider authenticated with the given credentials.
//
//	p := namecom.New("myuser", "mytoken")
func New(user, token string) *Provider {
	return &Provider{user: user, token: token}
}

type record struct {
	ID     int    `json:"id,omitempty"`
	Host   string `json:"host"`
	Type   string `json:"type"`
	Answer string `json:"answer"`
	TTL    int    `json:"ttl"`
}

// SetTXTRecord creates a _acme-challenge TXT record on name.com and returns
// the record ID (as a string) to be used later for cleanup.
func (p *Provider) SetTXTRecord(_ context.Context, domain, value string) (string, error) {
	zone := zoneFor(domain)
	host := challengeHost(domain, zone)

	rec := record{Host: host, Type: "TXT", Answer: value, TTL: 300}
	body, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("namecom: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/domains/%s/records", apiBase, zone)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(p.user, p.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("namecom: set record %s: %s: %s", resp.Status, domain, respBody)
	}

	var created record
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", fmt.Errorf("namecom: decode response: %w", err)
	}
	log.Printf("namecom: created TXT %s.%s (id=%d)", host, zone, created.ID)
	return fmt.Sprintf("%d", created.ID), nil
}

// DeleteTXTRecord removes a previously created TXT record by its ID.
func (p *Provider) DeleteTXTRecord(_ context.Context, domain, recordID string) error {
	zone := zoneFor(domain)
	url := fmt.Sprintf("%s/domains/%s/records/%s", apiBase, zone, recordID)

	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(p.user, p.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("namecom: delete record %s: %s: %s", recordID, resp.Status, data)
	}
	log.Printf("namecom: deleted TXT record id=%s for %s", recordID, domain)
	return nil
}

// zoneFor returns the registrable zone (last two labels).
// "tunnel.estellarx.com" → "estellarx.com"
func zoneFor(domain string) string {
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return domain
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// challengeHost builds the host field relative to the zone.
// domain="tunnel.estellarx.com", zone="estellarx.com" → "_acme-challenge.tunnel"
// domain="estellarx.com",        zone="estellarx.com" → "_acme-challenge"
func challengeHost(domain, zone string) string {
	prefix := strings.TrimSuffix(domain, "."+zone)
	if prefix == domain {
		return "_acme-challenge"
	}
	return "_acme-challenge." + prefix
}
