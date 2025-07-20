package shortmux

import "github.com/henvic/shortmux/internal/httpguts"

// isToken reports whether v is a valid token (https://www.rfc-editor.org/rfc/rfc2616#section-2.2).
func isToken(v string) bool {
	// For historical reasons, this function is called ValidHeaderFieldName (see issue #67031).
	return httpguts.ValidHeaderFieldName(v)
}
