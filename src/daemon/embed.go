package daemon

import _ "embed"

// IntrospectPython is materialized into each private worker runtime directory.
//
//go:embed introspect.py
var IntrospectPython []byte
