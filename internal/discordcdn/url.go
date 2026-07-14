// Package discordcdn centralizes the release policy for Discord attachment
// URLs. Keeping this in one place prevents the archive API, Firestore
// projector, and media fetchers from drifting into different SSRF policies.
package discordcdn

import (
	"net/url"
	"strings"
)

// AttachmentURL returns the canonical URL only when it is an HTTPS Discord
// attachment URL on an exact allowlisted host. Signed query parameters are
// retained, but userinfo, explicit ports, fragments, and non-attachment paths
// are rejected.
func AttachmentURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.Port() != "" || parsed.Fragment != "" ||
		!strings.HasPrefix(parsed.EscapedPath(), "/attachments/") {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "cdn.discordapp.com" && host != "media.discordapp.net" {
		return ""
	}
	return parsed.String()
}
