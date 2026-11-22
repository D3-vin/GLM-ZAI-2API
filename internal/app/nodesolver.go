// nodesolver — минтит captcha_verify_param для chat.z.ai через node-солвер.
//
// Зачем: device-token путь (tokens.sqlite + InitCaptchaV3/VerifyCaptchaV3)
// уперся в FRONTEND_CAPTCHA_REQUIRED: сервер требует параметр, собранный
// именно фронтендом (device VM + FeiLin + pe.* байткод). Пересборка параметра
// из device-token даёт F001/F017 — доказано замерами 22.11.
//
// Солвер — из проекта D3-vin/Zcode2Api (Educational Use License, тот же автор,
// что и наш мост). Запускается как отдельный процесс: node solver.js <scene>
// <region> <prefix>, печатает "VERIFY_PARAM=<param>" и выходит с 0.
//
// Ключевая грабля: pe-stall — динамический pe.* байткод Aliyun исполняется не
// всегда (наблюдено ~30-40% прогонов). Это НЕ поломка: автор апстрима прямо
// пишет "pe-stall on the first run is normal lifecycle, not failure" и делает
// retry. Замер 22.11: без retry 1/3, с retry 10/10 успешных минтов.
package app

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// captchaSceneID / region / prefix берутся из живого бандла chat.z.ai
	// (assets/captcha-*.js, проверено 22.11.2026): get SCENE_ID() возвращает
	// "didk33e0" для хоста chat.z.ai, иначе "xswyjefn".
	nodeSolverScene  = "didk33e0"
	nodeSolverRegion = "sgp"
	nodeSolverPrefix = "no8xfe"

	// nodeSolverRetries — сколько процессов запускать на один минт. pe-stall
	// лечится именно повтором; 5 с запасом перекрывает наблюдаемый worst case
	// (4 попытки из 6 в параллельном замере).
	nodeSolverRetries = 5

	// nodeSolverTimeout — один процесс солвера. Успех укладывается в 3.5-4.5s,
	// stall обрывается на ~9s, поэтому 90s — щедрый потолок, не узкое место.
	nodeSolverTimeout = 90 * time.Second
)

// NodeSolver минтит verify-параметры, запуская node-процесс.
type NodeSolver struct {
	dir     string        // каталог с solver.js и node_modules
	nodeBin string        // путь к node
	sem     chan struct{} // ограничитель параллельных минтов
	next    atomic.Uint32
	workers int
}

func newNodeSolver(dir string) *NodeSolver {
	nodeBin := "node"
	if p, err := exec.LookPath("node"); err == nil {
		nodeBin = p
	} else if _, statErr := os.Stat("/usr/bin/node"); statErr == nil {
		nodeBin = "/usr/bin/node"
	}
	workers := 2 // параллельные минты; каждый со своим CDN-кэшем
	return &NodeSolver{dir: dir, nodeBin: nodeBin, sem: make(chan struct{}, workers), workers: workers}
}

// Available сообщает, пригоден ли солвер: есть node, solver.js и зависимости.
func (n *NodeSolver) Available() bool {
	if n == nil || n.dir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(n.dir, "solver.js")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(n.dir, "node_modules", "happy-dom")); err != nil {
		return false
	}
	if _, err := exec.LookPath(n.nodeBin); err != nil && n.nodeBin == "node" {
		return false
	}
	return true
}

// cacheDir разводит CDN-кэш по воркерам: параллельные процессы в одном
// каталоге гоняются за запись/вытеснение и падают exit-1 (грабля апстрима).
func (n *NodeSolver) cacheDir() string {
	if n.workers <= 1 {
		return ""
	}
	i := int(n.next.Add(1)-1) % n.workers
	return filepath.Join(n.dir, "cdn-cache", fmt.Sprintf("w%d", i))
}

// Solve возвращает свежий captcha_verify_param или ошибку.
// Минт сериализован семафором; retry внутри покрывает pe-stall.
func (n *NodeSolver) Solve(ctx context.Context) (string, error) {
	if !n.Available() {
		return "", fmt.Errorf("node solver недоступен: нужен node + %s/solver.js + node_modules", n.dir)
	}
	select {
	case n.sem <- struct{}{}:
		defer func() { <-n.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	cache := n.cacheDir()
	if cache != "" {
		_ = os.MkdirAll(cache, 0o755)
	}

	var lastErr error
	for attempt := 1; attempt <= nodeSolverRetries; attempt++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		param, err := n.solveOnce(ctx, cache)
		if err == nil && param != "" {
			log.Printf("[Captcha] node-solver: param %d байт (попытка %d/%d)",
				len(param), attempt, nodeSolverRetries)
			return param, nil
		}
		if err == nil {
			err = fmt.Errorf("пустой VERIFY_PARAM")
		}
		lastErr = err
		log.Printf("[Captcha] node-solver попытка %d/%d: %v", attempt, nodeSolverRetries, err)
	}
	return "", fmt.Errorf("все %d попыток солвера провалились: %w", nodeSolverRetries, lastErr)
}

// solveOnce запускает один процесс солвера и читает VERIFY_PARAM из stdout.
func (n *NodeSolver) solveOnce(ctx context.Context, cache string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, nodeSolverTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, n.nodeBin,
		filepath.Join(n.dir, "solver.js"),
		nodeSolverScene, nodeSolverRegion, nodeSolverPrefix)
	cmd.Dir = n.dir
	// Прокси-переменные хоста вычищаем: солвер должен ходить напрямую, иначе
	// геолокация/egress не совпадут с теми, для которых минтится параметр.
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		k := strings.SplitN(kv, "=", 2)[0]
		switch k {
		case "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy":
			continue
		}
		env = append(env, kv)
	}
	if cache != "" {
		env = append(env, "CAPTCHA_CDN_CACHE="+cache)
	}
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = nil // солвер много шумит в stderr — не засоряем лог моста

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("запуск солвера: %w", err)
	}

	var param string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, "VERIFY_PARAM="); ok {
			param = strings.TrimSpace(v)
		}
	}
	werr := cmd.Wait()

	if param != "" {
		return param, nil // param важнее кода выхода
	}
	if runCtx.Err() != nil {
		return "", fmt.Errorf("таймаут солвера (%s)", nodeSolverTimeout)
	}
	if werr != nil {
		return "", fmt.Errorf("солвер вышел: %w", werr)
	}
	return "", fmt.Errorf("солвер не выдал VERIFY_PARAM (pe-stall)")
}

// ProbeNodeSolver — диагностическая точка входа для cmd/probe-solver:
// минтит параметр тем же кодом, что и прод, без поднятия сервера.
func ProbeNodeSolver() (string, error) {
	s := getNodeSolver()
	if s == nil {
		return "", fmt.Errorf("солвер не сконфигурирован")
	}
	return s.Solve(context.Background())
}

var (
	solverMu   sync.Mutex
	solverInst *NodeSolver
)

// getNodeSolver возвращает общий экземпляр солвера (nil, если не настроен).
// Каталог задается ZAI_NODE_SOLVER_DIR; по умолчанию — captcha_node рядом с бинарником.
func getNodeSolver() *NodeSolver {
	solverMu.Lock()
	defer solverMu.Unlock()
	if solverInst != nil {
		return solverInst
	}
	dir := os.Getenv("ZAI_NODE_SOLVER_DIR")
	if dir == "" {
		if exe, err := os.Executable(); err == nil {
			dir = filepath.Join(filepath.Dir(filepath.Dir(exe)), "captcha_node")
		}
	}
	if dir == "" {
		return nil
	}
	s := newNodeSolver(dir)
	solverInst = s
	if s.Available() {
		log.Printf("[Captcha] node-solver готов: %s (node=%s)", dir, s.nodeBin)
	} else {
		log.Printf("[Captcha] node-solver НЕ готов: %s (нет solver.js/node_modules/node)", dir)
	}
	return s
}
