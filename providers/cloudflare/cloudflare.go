// Package cloudflare implements providers.DNSProvider using the Cloudflare
// DNS API v4. Credentials are an API Token with "Zone / DNS / Edit" permission.
// Docs: https://developers.cloudflare.com/api/operations/dns-records-for-a-zone-create-dns-record
package cloudflare

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

const apiBase = "https://api.cloudflare.com/client/v4"

// Provider satisfies providers.DNSProvider for Cloudflare-managed domains.
type Provider struct {
	// apiToken is a scoped Cloudflare API token (preferred over Global API Key).
	apiToken string
}

// New returns a Cloudflare DNS provider.
// apiToken must have "Zone / DNS / Edit" permission for the target zone.
//
//	p := cloudflare.New("cf_api_token_here")
func New(apiToken string) *Provider {
	return &Provider{apiToken: apiToken}
}

// ─── Cloudflare API types ─────────────────────────────────────────────────────

type cfResult[T any] struct {
	Success bool     `json:"success"`
	Errors  []cfErr  `json:"errors"`
	Result  T        `json:"result"`
}

type cfErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cfRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
}

// ─── DNSProvider implementation ───────────────────────────────────────────────

// SetTXTRecord creates a _acme-challenge TXT record via the Cloudflare API.
// Returns the record ID for later deletion.
func (p *Provider) SetTXTRecord(ctx context.Context, domain, value string) (string, error) {
	zoneID, err := p.zoneID(ctx, domain)
	if err != nil {
		return "", fmt.Errorf("cloudflare: resolve zone for %s: %w", domain, err)
	}

	rec := cfRecord{
		Type:    "TXT",
		Name:    "_acme-challenge." + domain,
		Content: value,
		TTL:     120,
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("cloudflare: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/zones/%s/dns_records", apiBase, zoneID)
	var result cfResult[cfRecord]
	if err := p.do(ctx, http.MethodPost, url, bytes.NewReader(body), &result); err != nil {
		return "", fmt.Errorf("cloudflare: create record: %w", err)
	}
	if !result.Success {
		return "", fmt.Errorf("cloudflare: create record: %s", cfErrs(result.Errors))
	}

	log.Printf("cloudflare: created TXT _acme-challenge.%s (id=%s)", domain, result.Result.ID)
	return result.Result.ID, nil
}

// DeleteTXTRecord removes a previously created TXT record by its ID.
func (p *Provider) DeleteTXTRecord(ctx context.Context, domain, recordID string) error {
	zoneID, err := p.zoneID(ctx, domain)
	if err != nil {
		return fmt.Errorf("cloudflare: resolve zone for %s: %w", domain, err)
	}

	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", apiBase, zoneID, recordID)
	var result cfResult[json.RawMessage]
	if err := p.do(ctx, http.MethodDelete, url, nil, &result); err != nil {
		return fmt.Errorf("cloudflare: delete record %s: %w", recordID, err)
	}
	if !result.Success {
		return fmt.Errorf("cloudflare: delete record %s: %s", recordID, cfErrs(result.Errors))
	}

	log.Printf("cloudflare: deleted TXT record id=%s for %s", recordID, domain)
	return nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// zoneID resolves the Cloudflare zone ID for the registrable zone of domain.
// "tunnel.estellarx.com" → zone ID for "estellarx.com"
func (p *Provider) zoneID(ctx context.Context, domain string) (string, error) {
	zone := registrableZone(domain)
	url := fmt.Sprintf("%s/zones?name=%s&status=active", apiBase, zone)

	var result cfResult[[]cfZone]
	if err := p.do(ctx, http.MethodGet, url, nil, &result); err != nil {
		return "", err
	}
	if !result.Success || len(result.Result) == 0 {
		return "", fmt.Errorf("zone %q not found in Cloudflare account", zone)
	}
	return result.Result[0].ID, nil
}

func (p *Provider) do(ctx context.Context, method, url string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %s: %s", resp.Status, data)
	}
	return json.Unmarshal(data, out)
}

// registrableZone returns the last two labels of domain.
// "tunnel.estellarx.com" → "estellarx.com"
func registrableZone(domain string) string {
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return domain
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

func cfErrs(errs []cfErr) string {
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = fmt.Sprintf("%d: %s", e.Code, e.Message)
	}
	return strings.Join(msgs, "; ")
}
