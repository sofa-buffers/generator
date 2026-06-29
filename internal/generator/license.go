package generator

import "strings"

// LicenseID resolves the SPDX license identifier to stamp into generated file
// headers, from the generic `license` config option. It is a single generic
// option that controls every target's SPDX header.
//
//   - unset / "" / "none" -> "" — no SPDX line (the default: not everyone
//     licenses under MIT; some output is proprietary or unlicensed)
//   - "MIT", "Apache-2.0", "LicenseRef-Acme", … -> that identifier
//
// Backends emit the SPDX comment only when this returns a non-empty string.
func LicenseID(cfg map[string]any) string {
	v, _ := cfg["license"].(string)
	if strings.EqualFold(v, "none") {
		return ""
	}
	return v
}
