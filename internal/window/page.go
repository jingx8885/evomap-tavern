package window

import _ "embed"

//go:embed page.html
var embeddedPage []byte

func mustPage() []byte {
	return embeddedPage
}
