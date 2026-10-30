//go:build gcr_payload

package payload

import "embed"

//go:embed files/*.zst
var files embed.FS
