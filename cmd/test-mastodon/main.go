package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"time"
)

func main() {
	mastodonURL := flag.String("mastodon", "https://piaille.fr", "Mastodon instance URL")
	username := flag.String("user", "graft", "Mastodon username/handle")
	flag.Parse()

	messages := []string{
		"Synchronisation fédérée via Radicle pour la constitution française.",
		"Résilience par escrow sur plusieurs instances décentralisées.",
		"Historique complet des révisions de 1962 et 1960 préservé.",
		"Synchronisation bidirectionnelle Forgejo + Radicle.",
		"Métadonnées structurées pour chaque article constitutionnel ?",
		"Miroir fédéré vs plateformes centralisées.",
		"Coexistence harmonieuse de plates-formes décentralisées.",
		"Comment les révisions futures seront intégrées ?",
		"Accessibilité garantie via Radicle.",
		"Discussions structurées par article ?",
	}

	for i, msg := range messages {
		if err := postToMastodon(*mastodonURL, *username, msg); err != nil {
			fmt.Printf("Post %d FAILED: %v\n", i+1, err)
			continue
		}
		fmt.Printf("Post %d OK\n", i+1)
		time.Sleep(1 * time.Second)
	}
}

func postToMastodon(instanceURL, username, text string) error {
	// Generate a minimal Create{Note} activity
	noteID := fmt.Sprintf("%s/notes/%d", instanceURL, time.Now().UnixNano())
	actorID := fmt.Sprintf("%s/users/%s", instanceURL, username)

	note := map[string]interface{}{
		"@context":     "https://www.w3.org/ns/activitystreams",
		"type":         "Note",
		"id":           noteID,
		"content":      text,
		"attributedTo": actorID,
		"published":    time.Now().UTC().Format(time.RFC3339),
		"to":           []string{"https://www.w3.org/ns/activitystreams#Public"},
	}

	create := map[string]interface{}{
		"@context":  "https://www.w3.org/ns/activitystreams",
		"type":      "Create",
		"id":        fmt.Sprintf("%s/create/%d", instanceURL, time.Now().UnixNano()),
		"actor":     actorID,
		"object":    note,
		"published": time.Now().UTC().Format(time.RFC3339),
		"to":        []string{"https://www.w3.org/ns/activitystreams#Public"},
	}

	body, err := json.Marshal(create)
	if err != nil {
		return err
	}

	// POST to Mastodon's sharedInbox or user inbox
	url := fmt.Sprintf("%s/inbox", instanceURL)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/activity+json")
	req.Header.Set("User-Agent", "graft/1.0 (+https://github.com/jeanbaptiste/graft)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
