// Package api embeds the canonical HTTP contract. The YAML file is the source
// of truth for request and response shapes; this package only makes it
// available to the binary without a runtime filesystem dependency.
package api

import _ "embed"

// OpenAPI is the OpenAPI 3.0.3 document served by the documentation adapter.
//
//go:embed openapi.yaml
var OpenAPI []byte
