// Command romm-tui is the mister-romm terminal application (see internal/app).
package main

import "github.com/tvdu29/mister-romm/src/internal/app"

// version is set at build time with -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() { app.Run(version) }
