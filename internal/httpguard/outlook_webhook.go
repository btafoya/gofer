package httpguard

import (
	"errors"
	"net/netip"
	"strings"
)

// OutlookWebhookPath receives Outlook change notifications from Microsoft
// Graph when GOFER_OUTLOOK_WEBHOOKS is enabled. Microsoft's servers send no
// session or origin, so the endpoint skips those checks and accepts only
// notifications that carry a subscription's own secret. GOFER_ALLOWED_CIDRS
// still applies unless GOFER_OUTLOOK_WEBHOOKS_SKIP_ALLOWED_CIDRS is set.
const OutlookWebhookPath = "/webhooks/outlook"

// OutlookWebhookURL is where Graph sends notifications, or empty when
// GOFER_OUTLOOK_WEBHOOKS is off.
func (c *Config) OutlookWebhookURL() string {
	if !c.OutlookWebhooks {
		return ""
	}
	return c.BaseURL + OutlookWebhookPath
}

// PostmarkWebhookPathBase prefixes the per-account webhook routes:
// /webhooks/postmark/inbound/{accountID}/{secret} and
// /webhooks/postmark/events/{accountID}/{secret}. Postmark's servers send no
// session either; each route carries its account ID and a random secret, and
// the signed body must match the account's webhook secret.
const PostmarkWebhookPathBase = "/webhooks/postmark/"

// PostmarkInboundWebhookURL and PostmarkEventsWebhookURL build the URLs an
// account's Postmark server is configured to call.
func (c *Config) PostmarkInboundWebhookURL(accountID, secret string) string {
	return c.BaseURL + PostmarkWebhookPathBase + "inbound/" + accountID + "/" + secret
}
func (c *Config) PostmarkEventsWebhookURL(accountID, secret string) string {
	return c.BaseURL + PostmarkWebhookPathBase + "events/" + accountID + "/" + secret
}

func (c *Config) isPostmarkWebhook(path string) bool {
	return strings.HasPrefix(path, PostmarkWebhookPathBase)
}

func (c *Config) isOutlookWebhook(path string) bool {
	return c.OutlookWebhooks && path == OutlookWebhookPath
}

// WarnOutlookWebhooksBehindAllowlist reports whether GOFER_ALLOWED_CIDRS may
// turn away Microsoft's servers, because skipping it wasn't confirmed.
func (c *Config) WarnOutlookWebhooksBehindAllowlist() bool {
	return c.OutlookWebhooks && len(c.allowedCIDRs) > 0 && !c.OutlookWebhooksSkipAllowedCIDRs
}

// validateOutlookWebhooks checks that Microsoft could reach the endpoint at
// all: Graph only sends to public HTTPS addresses.
func (c *Config) validateOutlookWebhooks(authEnabled bool) error {
	if !c.OutlookWebhooks {
		if c.OutlookWebhooksSkipAllowedCIDRs {
			return errors.New("GOFER_OUTLOOK_WEBHOOKS_SKIP_ALLOWED_CIDRS requires GOFER_OUTLOOK_WEBHOOKS=true")
		}
		return nil
	}
	if !authEnabled {
		return errors.New("GOFER_OUTLOOK_WEBHOOKS requires personal or managed authentication")
	}
	public := c.baseOrigin.scheme == "https" && !c.baseLoopback
	if ip, err := netip.ParseAddr(normalizeHost(c.baseOrigin.host)); err == nil {
		public = public && ip.IsGlobalUnicast() && !ip.IsPrivate()
	}
	if !public {
		return errors.New("GOFER_OUTLOOK_WEBHOOKS requires GOFER_BASE_URL to be a public https address")
	}
	return nil
}
