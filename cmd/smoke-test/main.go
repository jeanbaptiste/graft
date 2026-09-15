package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type Note struct {
	Context      string   `json:"@context"`
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	AttributedTo string   `json:"attributedTo"`
	InReplyTo    string   `json:"inReplyTo"`
	Content      string   `json:"content"`
	URL          string   `json:"url"`
	Published    string   `json:"published"`
	To           []string `json:"to"`
}

type Create struct {
	Context   string   `json:"@context"`
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Actor     string   `json:"actor"`
	Published string   `json:"published"`
	To        []string `json:"to"`
	Object    Note     `json:"object"`
}

var (
	graftHost = flag.String("graft", "https://f1.cyberwild.org", "Graft base URL")
	series    = flag.String("series", "constitution", "Series name")
	issueURI  = flag.String("issue", "", "Issue note URI to reply to")
	actorURL  = flag.String("actor", "https://bridge.test/actor", "Bridge actor URL")
	numMsgs   = flag.Int("num", 20, "Number of messages to create")
	fromURL   = flag.String("from", "", "Platform source URL (e.g. discourse topic URL)")
)

// Message templates for each repository
var templates = []string{
	"Miroir fédéré via Radicle pour la constitution française avec graft",
	"Résilience par escrow sur plusieurs instances décentralisées de graft",
	"Historique complet des révisions de 1962 et 1960 préservé via graft",
	"Synchronisation bidirectionnelle Forgejo + Radicle avec traçabilité graft",
	"Métadonnées structurées pour chaque article constitutionnel dans graft",
	"Miroir fédéré vs plateformes centralisées : avantages et inconvénients",
	"Coexistence harmonieuse de plates-formes décentralisées avec graft",
	"Comment les révisions futures seront intégrées via graft ?",
	"Accessibilité garantie via Radicle et graft pour tous les utilisateurs",
	"Discussions structurées par article via graft synchronisation",
	"Federation-x : pont entre Discourse, Zulip et Tangled via graft",
	"Architecture mesh : comment graft relie les systèmes",
	"Transparence des décisions via votes sur Radicle enregistrés par graft",
	"Signatures cryptographiques garantissent l'intégrité via graft ActivityPub",
	"Backup distribué : chaque nœud Radicle = copie complète + historique",
	"Graft indexe tous les commentaires cross-plateforme pour recherche unifiée",
	"Comment contribuer localement sans accès Internet centralisé graft",
	"Performance : synchronisation near-real-time entre instances graft",
	"Sécurité : validation cryptographique de tous les posts relayés par graft",
	"Documentation multilingue maintenue via graft et Radicle",
}

func main() {
	flag.Parse()

	if *issueURI == "" {
		log.Fatal("--issue is required")
	}

	client := &http.Client{Timeout: 10 * time.Second}

	// Try to resolve the inbox URL from the series actor
	fmt.Printf("Fetching actor document for %s...\n", *series)
	actorDoc, err := fetchActor(client, *graftHost, *series)
	var inbox string
	if err != nil {
		fmt.Printf("⚠️  Cannot fetch actor: %v\n", err)
		fmt.Printf("   Using fallback inbox URL...\n")
		inbox = fmt.Sprintf("%s/actors/%s/inbox", strings.TrimRight(*graftHost, "/"), *series)
	} else {
		fmt.Printf("✓ Actor inbox: %s\n", actorDoc.Inbox)
		inbox = actorDoc.Inbox
	}
	fmt.Printf("✓ Target inbox: %s\n\n", inbox)

	now := time.Now().UTC()
	startNano := now.UnixNano()

	// Create and send messages
	for i := 0; i < *numMsgs; i++ {
		template := templates[i%len(templates)]
		sourceURL := *fromURL
		if sourceURL == "" {
			sourceURL = fmt.Sprintf("https://discourse.cyberwild.org/t/smoke-test-%d/%d", i+1, i+1)
		}

		nanoID := startNano + int64(i)*1000
		actorID := fmt.Sprintf("%s/actor-%d", *actorURL, i)
		noteID := fmt.Sprintf("%s/notes/%d", actorID, nanoID)

		note := Note{
			Context:      "https://www.w3.org/ns/activitystreams",
			ID:           noteID,
			Type:         "Note",
			AttributedTo: actorID,
			InReplyTo:    *issueURI,
			Content:      fmt.Sprintf("%s (msg %d/%d)", template, i+1, *numMsgs),
			URL:          sourceURL,
			Published:    time.Unix(0, nanoID).UTC().Format(time.RFC3339),
			To:           []string{"https://www.w3.org/ns/activitystreams#Public"},
		}

		create := Create{
			Context:   "https://www.w3.org/ns/activitystreams",
			ID:        noteID + "/activity",
			Type:      "Create",
			Actor:     actorID,
			Published: note.Published,
			To:        []string{"https://www.w3.org/ns/activitystreams#Public"},
			Object:    note,
		}

		body, err := json.Marshal(create)
		if err != nil {
			fmt.Printf("✗ Message %d/%d: marshal failed\n", i+1, *numMsgs)
			continue
		}

		if err := postActivity(client, actorDoc.Inbox, body); err != nil {
			fmt.Printf("✗ Message %d/%d: POST failed: %v\n", i+1, *numMsgs, err)
			continue
		}
		fmt.Printf("✓ Message %d/%d: sent (URL: %s)\n", i+1, *numMsgs, sourceURL)
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Printf("\n✓ All %d messages sent\n", *numMsgs)
	fmt.Printf("Waiting for wiki generation...\n")
	time.Sleep(5 * time.Second)

	// Test wiki generation
	fmt.Printf("\nChecking wiki pages...\n")
	wikiURLs := []string{
		fmt.Sprintf("https://f1.cyberwild.org/wiki/%s/social-discourse.html", *series),
		fmt.Sprintf("https://f1.cyberwild.org/wiki/%s/social-zulip.html", *series),
		fmt.Sprintf("https://f1.cyberwild.org/wiki/%s/social-tangled.html", *series),
	}

	for _, wikiURL := range wikiURLs {
		checkWiki(client, wikiURL)
	}
}

type Actor struct {
	Inbox string `json:"inbox"`
}

func fetchActor(client *http.Client, graftHost, series string) (*Actor, error) {
	actorURL := fmt.Sprintf("%s/actors/%s", strings.TrimRight(graftHost, "/"), series)
	req, _ := http.NewRequest("GET", actorURL, nil)
	req.Header.Set("Accept", "application/activity+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}

	var actor Actor
	if err := json.NewDecoder(resp.Body).Decode(&actor); err != nil {
		return nil, err
	}
	return &actor, nil
}

func postActivity(client *http.Client, inbox string, body []byte) error {
	req, _ := http.NewRequest("POST", inbox, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/activity+json")
	req.Header.Set("User-Agent", "graft-smoke-test/1.0")

	// Sign with a fake signature (graft should accept unsigned for testing)
	sigHeader := fmt.Sprintf(`keyId="%s", algorithm="rsa-sha256", headers="(request-target) host date", signature="fake"`, "https://bridge.test/actor#main-key")
	req.Header.Set("Signature", sigHeader)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func checkWiki(client *http.Client, wikiURL string) {
	fmt.Printf("\n%s:\n", wikiURL)

	resp, err := client.Get(wikiURL)
	if err != nil {
		fmt.Printf("  ✗ Fetch failed: %v\n", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// Check for trackbacks
	trackbackCount := strings.Count(html, "[→")
	fmt.Printf("  • Trackback links found: %d\n", trackbackCount)

	// Check for footer
	if strings.Contains(html, "Page") && strings.Contains(html, "par graft") {
		fmt.Printf("  • Footer present: yes\n")
		// Extract timestamp
		startIdx := strings.Index(html, "Page")
		if startIdx != -1 {
			endIdx := strings.Index(html[startIdx:], "_") + startIdx
			footer := html[startIdx:endIdx]
			fmt.Printf("    %s\n", footer)
		}
	} else {
		fmt.Printf("  • Footer present: no\n")
	}

	// Spider trackback links
	links := extractTrackbackLinks(html)
	if len(links) > 0 {
		fmt.Printf("  • Trackback URLs:\n")
		for i, link := range links {
			if i >= 5 {
				fmt.Printf("    ... and %d more\n", len(links)-5)
				break
			}
			linkValid := validateURL(client, link)
			status := "✓"
			if !linkValid {
				status = "✗"
			}
			fmt.Printf("    %s %s\n", status, link)
		}
	}
}

func extractTrackbackLinks(html string) []string {
	var links []string
	idx := 0
	for {
		idx = strings.Index(html[idx:], "[→")
		if idx == -1 {
			break
		}
		idx += 2
		// Find the closing ](url)
		closeIdx := strings.Index(html[idx:], "](")
		if closeIdx == -1 {
			break
		}
		closeIdx += idx + 2
		urlEnd := strings.Index(html[closeIdx:], ")")
		if urlEnd == -1 {
			break
		}
		url := html[closeIdx : closeIdx+urlEnd]
		if url != "" && (strings.HasPrefix(url, "http") || strings.HasPrefix(url, "https")) {
			links = append(links, url)
		}
	}
	return links
}

func validateURL(client *http.Client, targetURL string) bool {
	// Don't validate external URLs to avoid timeouts
	if strings.Contains(targetURL, "discourse.cyberwild.org") ||
		strings.Contains(targetURL, "zulip.cyberwild.org") ||
		strings.Contains(targetURL, "bsky.app") {
		// Quick HEAD check
		req, _ := http.NewRequest("HEAD", targetURL, nil)
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode < 500
	}
	return true
}
