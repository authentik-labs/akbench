// Package seeder embeds the tenant seeder script, so the operator binary
// carries it and can hand it to a seeding Job (see
// render.TenantSeederConfigMap) without a separate image.
package seeder

import _ "embed"

// Script is seed.py, run inside a deployment's own authentik image.
//
//go:embed seed.py
var Script string
