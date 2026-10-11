// Package web holds the website the server serves at /.
package web

import _ "embed"

//go:embed index.html
var Index []byte
