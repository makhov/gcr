//go:build !gcr_payload

package payload

import "embed"

// A build without the gcr_payload tag carries nothing.
var files embed.FS
