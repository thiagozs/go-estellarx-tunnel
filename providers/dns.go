// Package providers defines the DNSProvider interface used by the wildcard
// certificate manager to complete ACME DNS-01 challenges. Any DNS registrar
// can be supported by implementing this two-method interface.
package providers

import "context"

// DNSProvider is implemented by any DNS backend capable of creating and
// deleting TXT records on behalf of the ACME DNS-01 challenge flow.
//
// Implementations must be safe for concurrent use.
type DNSProvider interface {
	// SetTXTRecord creates a _acme-challenge TXT record for the given domain
	// with the given value. It returns an opaque recordID that must be passed
	// to DeleteTXTRecord once the challenge is complete.
	SetTXTRecord(ctx context.Context, domain, value string) (recordID string, err error)

	// DeleteTXTRecord removes the TXT record previously created by SetTXTRecord.
	DeleteTXTRecord(ctx context.Context, domain, recordID string) error
}
