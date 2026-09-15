package main

import (
	"flag"
	"fmt"
	"log"

	"graft/internal/atproto"
)

func main() {
	handle := flag.String("handle", "", "Bluesky handle (e.g. constitution.pds.cyberwild.org)")
	appPassword := flag.String("password", "", "Bluesky app password")
	pds := flag.String("pds", "https://pds.cyberwild.org", "PDS host URL")
	flag.Parse()

	if *handle == "" || *appPassword == "" {
		log.Fatal("usage: test-bluesky -handle <handle> -password <password> [-pds <url>]")
	}

	client := atproto.New(*pds)
	if err := client.Login(*handle, *appPassword); err != nil {
		log.Fatalf("login failed: %v", err)
	}

	messages := []string{
		"Synchronisation fédérée via Radicle pour la constitution française.",
		"Résilience par escrow sur plusieurs instances décentralisées.",
		"Historique complet des révisions de 1962 et 1960 préservé.",
		"Synchronisation bidirectionnelle Forgejo + Radicle pour document fondamental.",
		"Métadonnées structurées pour chaque article constitutionnel ?",
		"Miroir fédéré vs plateformes centralisées - quel avenir ?",
		"Coexistence harmonieuse de plates-formes décentralisées via graft.",
		"Comment les révisions constitutionnelles futures seront intégrées ?",
		"Accessibilité garantie via Radicle même en cas de défaillance.",
		"Discussions structurées par article pour améliorer la gouvernance ?",
	}

	for i, msg := range messages {
		uri, err := client.Post(msg)
		if err != nil {
			fmt.Printf("Post %d FAILED: %v\n", i+1, err)
			continue
		}
		fmt.Printf("Post %d OK: %s\n", i+1, uri)
	}
}
