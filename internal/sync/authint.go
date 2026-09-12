package sync

import (
	"strings"

	"graft/internal/forgejo"
)

// HasAuthorizedIntegration reports whether fc's repository has a Forgejo
// Actions workflow configured to push directly to targetHost via
// Authorized Integrations (a job with `enable-openid-connect: true` whose
// body references targetHost). Detected by scanning
// .forgejo/workflows/*.yml — there's no admin API to query the trust
// relationship itself, so this is the only signal available from a
// regular repo token's perspective. False on any error (missing
// directory, no access, ...): absence of evidence is treated as "not
// configured", never as a reason to fail the sync.
func HasAuthorizedIntegration(fc *forgejo.Client, targetHost string) bool {
	names, err := fc.ListWorkflowFiles()
	if err != nil {
		return false
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		content, err := fc.GetFileContent(".forgejo/workflows/" + name)
		if err != nil {
			continue
		}
		if strings.Contains(content, "enable-openid-connect") && strings.Contains(content, targetHost) {
			return true
		}
	}
	return false
}
