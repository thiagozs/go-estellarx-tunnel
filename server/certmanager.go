package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const activityFile = "cert-activity.json"

// certManager tracks subdomain certificate usage and cleans up stale cache entries.
// Note: deleting cert cache entries does NOT free Let's Encrypt rate-limit slots
// (those are based on issuance history, not current cache state). However, keeping
// the cache clean prevents future accumulation and ensures autocert reuses certs
// for recurring subdomains instead of issuing new ones.
type certManager struct {
	mu         sync.Mutex
	cacheDir   string
	baseDomain string
	ttl        time.Duration
	activity   map[string]time.Time // subdomain -> last seen
}

func newCertManager(cacheDir, baseDomain string, ttl time.Duration) *certManager {
	cm := &certManager{
		cacheDir:   cacheDir,
		baseDomain: baseDomain,
		ttl:        ttl,
		activity:   make(map[string]time.Time),
	}
	cm.load()
	return cm
}

// touch updates the last-seen timestamp for a subdomain.
func (cm *certManager) touch(subdomain string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.activity[subdomain] = time.Now()
}

// load reads the persisted activity map from disk.
func (cm *certManager) load() {
	path := filepath.Join(cm.cacheDir, activityFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return // first run or file missing, start fresh
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()
	raw := make(map[string]int64)
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("certmanager: failed to parse activity file: %v", err)
		return
	}
	for sub, ts := range raw {
		cm.activity[sub] = time.Unix(ts, 0)
	}
	log.Printf("certmanager: loaded activity for %d subdomains", len(cm.activity))
}

// save persists the activity map to disk.
func (cm *certManager) save() {
	cm.mu.Lock()
	raw := make(map[string]int64, len(cm.activity))
	for sub, t := range cm.activity {
		raw[sub] = t.Unix()
	}
	cm.mu.Unlock()

	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		log.Printf("certmanager: marshal error: %v", err)
		return
	}
	if err := os.MkdirAll(cm.cacheDir, 0700); err != nil {
		log.Printf("certmanager: mkdir %s: %v", cm.cacheDir, err)
		return
	}
	path := filepath.Join(cm.cacheDir, activityFile)
	if err := os.WriteFile(path, data, 0600); err != nil {
		log.Printf("certmanager: write activity file: %v", err)
	}
}

// cleanup scans the cert cache directory and deletes files for subdomains
// that have not been seen for longer than the configured TTL.
func (cm *certManager) cleanup() {
	cm.mu.Lock()
	now := time.Now()
	stale := []string{}
	for sub, last := range cm.activity {
		if now.Sub(last) > cm.ttl {
			stale = append(stale, sub)
		}
	}
	cm.mu.Unlock()

	removed := 0
	for _, sub := range stale {
		domain := sub + "." + cm.baseDomain
		// autocert DirCache stores two variants: ECDSA (plain) and RSA (+rsa)
		for _, key := range []string{domain, domain + "+rsa"} {
			path := filepath.Join(cm.cacheDir, key)
			if err := os.Remove(path); err != nil {
				if !os.IsNotExist(err) {
					log.Printf("certmanager: remove %q: %v", key, err)
				}
				continue
			}
			log.Printf("certmanager: removed stale cert cache for %q (inactive %s)",
				sub, now.Sub(cm.activity[sub]).Round(time.Hour))
			removed++
		}
		cm.mu.Lock()
		delete(cm.activity, sub)
		cm.mu.Unlock()
	}

	// Also sweep the cache dir for cert files with no activity record (orphans).
	orphans := cm.removeOrphans()
	removed += orphans

	if removed == 0 && len(stale) == 0 {
		log.Printf("certmanager: cleanup complete — no stale entries found")
	} else {
		log.Printf("certmanager: cleanup complete — removed %d cert file(s) (%d stale subdomains, %d orphans)",
			removed, len(stale), orphans)
	}

	cm.save()
}

// removeOrphans deletes cert files for subdomains with no activity record at all.
// Returns the count of files removed.
func (cm *certManager) removeOrphans() int {
	entries, err := os.ReadDir(cm.cacheDir)
	if err != nil {
		log.Printf("certmanager: readdir %s: %v", cm.cacheDir, err)
		return 0
	}

	suffix := "." + cm.baseDomain
	removed := 0
	cm.mu.Lock()
	defer cm.mu.Unlock()

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == activityFile {
			continue
		}
		// Determine the subdomain from the filename.
		cleanName, _ := strings.CutSuffix(e.Name(), "+rsa")
		sub, ok := strings.CutSuffix(cleanName, suffix)
		if !ok || sub == "" {
			continue
		}
		if _, tracked := cm.activity[sub]; tracked {
			continue // has an activity record, not an orphan
		}
		path := filepath.Join(cm.cacheDir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("certmanager: remove orphan %q: %v", name, err)
			continue
		}
		log.Printf("certmanager: removed orphan cert cache: %q", name)
		removed++
	}
	return removed
}

// report logs the current state of the cert cache.
func (cm *certManager) report() {
	entries, err := os.ReadDir(cm.cacheDir)
	certCount := 0
	if err == nil {
		suffix := "." + cm.baseDomain
		seen := make(map[string]struct{})
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name, _ := strings.CutSuffix(e.Name(), "+rsa")
			if sub, ok := strings.CutSuffix(name, suffix); ok && sub != "" {
				seen[sub] = struct{}{}
			}
		}
		certCount = len(seen)
	}

	cm.mu.Lock()
	activityCount := len(cm.activity)
	cm.mu.Unlock()

	const leLimit = 50
	log.Printf("certmanager: %d unique subdomain cert(s) in cache, %d activity record(s) tracked [LE limit: %d/7d]",
		certCount, activityCount, leLimit)
	if certCount >= leLimit-5 {
		log.Printf("certmanager: WARNING — cert cache count (%d) is near Let's Encrypt rate limit (%d/7d). "+
			"Consider using a wildcard certificate (requires DNS-01 challenge).", certCount, leLimit)
	}
}

// runCleanup starts a background goroutine that periodically cleans up stale cert files.
func (cm *certManager) runCleanup(interval time.Duration) {
	go func() {
		cm.report()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			log.Printf("certmanager: running periodic cleanup (ttl=%s, interval=%s)", cm.ttl, interval)
			cm.cleanup()
			cm.report()
		}
	}()
}
