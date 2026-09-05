// cmd/server — thin entry point for the GLM-ZAI-2API bridge.
// All logic lives in internal/app (package app); run from the project root
// so .env / tokens.sqlite / proxy.txt resolve relative to the cwd.
package main

import app "glm-zai-2api/internal/app"

func main() {
	app.Main()
}
