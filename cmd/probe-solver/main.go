// Живая проверка: getNodeSolver() из нового кода моста минтит параметр.
// Запуск: go run ./internal/app/probe_note_solver_test.go — не годится,
// поэтому отдельный пакет-проба в cmd/.
package main

import (
	"fmt"
	"os"

	"glm-zai-2api/internal/app"
)

func main() {
	os.Setenv("ZAI_NODE_SOLVER_DIR", "/root/GLM-ZAI-2API/captcha_node")
	p, err := app.ProbeNodeSolver()
	if err != nil {
		fmt.Println("ОШИБКА:", err)
		os.Exit(1)
	}
	fmt.Printf("OK: param %d байт\n", len(p))
}
