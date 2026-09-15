package main

import (
	"flag"
	"fmt"
	"log"

	"graft/internal/atproto"
)

func main() {
	handle := flag.String("handle", "", "Tangled handle (e.g. constitution.pds.cyberwild.org)")
	appPassword := flag.String("password", "", "Tangled app password")
	pds := flag.String("pds", "https://pds.cyberwild.org", "PDS host URL")
	flag.Parse()

	if *handle == "" || *appPassword == "" {
		log.Fatal("usage: test-tangled -handle <handle> -password <password> [-pds <url>]")
	}

	client := atproto.New(*pds)
	if err := client.Login(*handle, *appPassword); err != nil {
		log.Fatalf("login failed: %v", err)
	}

	messages := []string{
		"Synchronisation fédérée constitution texte fondamental français.",
		"Radicle garantit résilience et escrow multi-instances.",
		"Historique révisions 1962 1960 preservation complète.",
		"Bidirectionnelle Forgejo Radicle document fondamental.",
		"Métadonnées structurées articles constitutionnels.",
		"Miroir fédéré vs plateformes centralisées.",
		"Coexistence plates-formes décentralisées harmonieuse.",
		"Futures révisions intégration système versioning.",
		"Accessibilité Radicle garantie cas defaillance.",
		"Discussions article constitution gouvernance améliorée.",
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
