package daemon

import _ "embed"

// IntrospectPython and ProtocolPython are materialized together into each private
// worker runtime directory; introspect.py imports protocol.py as a sibling module.
//
//go:embed introspect.py
var IntrospectPython []byte

//go:embed protocol.py
var ProtocolPython []byte
