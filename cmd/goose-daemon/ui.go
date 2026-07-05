package main

// ===== 학습 노트 (anvil v0.5.x 학습용 주석, 참고 전용 브랜치) =====
// 이 파일은 upstream ephemera v0.5.0에서 들어온 "임베디드 Web console" 서빙 계층이다.
// web/(Svelte + Vite)에서 빌드한 정적 SPA 산출물을 go:embed로 바이너리에 통째로 박아
// /ui/ 경로로 그대로 서빙한다 — 별도 정적 파일 서버나 CDN 없이 daemon 프로세스 하나가
// API와 UI를 같은 origin(CORS 불필요)에서 함께 낸다.
//
// anvil 적응 포인트(auth 경계): /ui/ 는 cmd/goose-daemon/api.go의 authMiddleware 체인
// 밖에 있다. 로그인 화면과 JS 번들 자체는 토큰이 없는 상태에서도 로드돼야 하고, 번들
// 안에는 어떤 비밀도 들어있지 않기 때문이다(토큰은 web/src/lib/api.js가 클라이언트
// sessionStorage/localStorage에만 보관). 로그인 이후 실제 데이터 호출(/vms, /flocks,
// /config/* 등)은 전부 authMiddleware 뒤 API로 나간다 — "정적 파일 + 로그인 화면만
// auth 밖" 이라는 경계는 cmd/goose-daemon/config_api.go의 모든 /config/* 데이터 API가
// 여전히 bearer 뒤에 있는 것과 대비된다.
//
// sentinel 가드 테스트(ui_test.go): TestUIAuthExempt(/ui/ 는 토큰 없이 200),
// TestAPIStillProtected(그 외 API는 여전히 401), TestRootRedirectsToUI, TestUISPAFallback,
// TestUIMissingAsset404.
//
// path traversal 방어: 서빙 대상이 os.DirFS가 아니라 embed.FS(uiSubFS)이므로 애초에
// "../"로 컨테이너 밖 호스트 파일에 닿을 방법이 없고, uiHandler 내부에서도 path.Clean으로
// 한 번 더 정규화한다 — 이중 방어.

import (
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
)

// [학습] go:embed all:uidist: uidist/ 는 web/에서 `npm run build`로 만든 산출물이 그대로
// git에 커밋되는 디렉터리다. 덕분에 CI/e2e 스크립트는 Node 툴체인 없이 `go build`만으로
// 완결된 UI가 포함된 바이너리를 만든다. all: 접두사가 없으면 Vite가 만드는 해시 청크 중
// '_'나 '.'로 시작하는 파일을 embed가 조용히 누락시킬 수 있어 명시했다.
// embeddedUI holds the built Web UI bundle (Vite output under uidist/). The
// frontend source lives in web/; `npm run build` writes its output here and the
// result is committed so `go build` works without a Node toolchain (CI and the
// e2e harness are node-free). See web/README.md for the rebuild step.
//
// The `all:` prefix includes files Vite emits with a leading '_' or '.'
// (e.g. hashed chunk names), which the default embed pattern would skip.
//
//go:embed all:uidist
var embeddedUI embed.FS

// [학습] fs.Sub로 embed.FS 루트를 uidist/ 아래로 재설정한다 — 이후 코드가 다루는 경로는
// "uidist/index.html"이 아니라 "index.html"처럼 번들 루트 기준 상대경로가 된다.
// uiSubFS roots the embedded filesystem at the uidist/ directory so paths are
// served relative to the bundle root (e.g. "index.html", "assets/...").
func uiSubFS() (fs.FS, error) {
	return fs.Sub(embeddedUI, "uidist")
}

// [학습] 개발자가 web/ 빌드를 깜빡 잊어도(placeholder uidist/만 있거나 아예 비어 있어도)
// go build 자체는 절대 실패하지 않는다는 설계 원칙을 지키는 검사 함수. 대신 uiHandler가
// 아래에서 경고 로그만 남기고 계속 서빙한다.
// embeddedUIPresent reports whether a real UI bundle was built into the binary.
// When only the committed placeholder (or nothing) is present the daemon logs a
// warning and still serves what it has — the build never fails on a missing UI.
func embeddedUIPresent(sub fs.FS) bool {
	f, err := sub.Open("index.html")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// [학습] 이 함수가 만드는 http.Handler가 externalMux에 "/ui/"로 등록되는 지점이며
// (api.go의 NewControlPlane 참고), authMiddleware를 감싸지 않는 유일한 데이터 서빙
// 경로다. 내부적으로 세 갈래로 분기한다: (1) 번들 루트("") → index.html, (2) 실제 존재하는
// 정적 파일 → http.FileServer로 그대로 서빙, (3) 존재하지 않는 경로 → asset처럼 보이면
// 404, 아니면 SPA 클라이언트 라우트로 간주해 index.html을 200으로 서빙(새로고침 시
// /ui/vms/<id> 같은 딥링크가 살아남는 이유).
// uiHandler serves the embedded single-page app under the /ui/ prefix.
//
// It is mounted OUTSIDE the auth/audit middleware chain on purpose: the login
// screen and JS bundle must load before the user has a Bearer token, and the
// bundle carries no secrets (the token lives only client-side). Every data API
// call the SPA then makes (/vms, /flocks, ...) still passes through
// authMiddleware unchanged.
//
// Unknown sub-paths that are not asset requests fall back to index.html so that
// client-side routes (e.g. /ui/vms/<id>) survive a page reload.
//
// Note: net/http.FileServerFS landed in Go 1.22; this uses http.FileServer +
// http.FS to stay compatible with the go.mod 1.21 declaration.
func (cp *ControlPlane) uiHandler() http.Handler {
	sub, err := uiSubFS()
	if err != nil {
		slog.Warn("ui: failed to open embedded bundle, serving 404", "err", err)
		return http.NotFoundHandler()
	}
	if !embeddedUIPresent(sub) {
		slog.Warn("ui: no index.html in embedded bundle; run `cd web && npm run build` to populate uidist/")
	}

	fileServer := http.StripPrefix("/ui/", http.FileServer(http.FS(sub)))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Resolve the path relative to the bundle root.
		rel := strings.TrimPrefix(r.URL.Path, "/ui/")
		rel = path.Clean("/" + rel)[1:] // strip leading slash, clean ../ etc.

		// Serve the bundle root directly.
		if rel == "" {
			r.URL.Path = "/ui/"
			fileServer.ServeHTTP(w, r)
			return
		}

		// If the requested file exists in the bundle, serve it as-is.
		if _, statErr := fs.Stat(sub, rel); statErr == nil {
			fileServer.ServeHTTP(w, r)
			return
		} else if !os.IsNotExist(statErr) {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Missing file: asset requests 404 honestly; everything else is a
		// client-side route → serve index.html so a reload works.
		if isAssetRequest(rel) {
			http.NotFound(w, r)
			return
		}
		serveIndex(w, r, sub)
	})
}

// [학습] "존재하지 않는 경로"를 두 종류로 나누는 휴리스틱: assets/ 아래이거나 확장자가
// 있으면 진짜 정적 파일 요청(오타·삭제된 파일)으로 보고 정직하게 404를, 그 외(확장자
// 없는 경로)는 SPA 클라이언트 사이드 라우트로 보고 index.html로 폴백한다.
// isAssetRequest reports whether a missing path looks like a static asset
// (under assets/ or carrying a file extension) rather than an SPA route.
func isAssetRequest(rel string) bool {
	if strings.HasPrefix(rel, "assets/") {
		return true
	}
	return path.Ext(rel) != ""
}

// [학습] SPA fallback의 실제 구현. 항상 200으로 index.html을 반환해야 브라우저가
// "정상 페이지"로 받아들이고 클라이언트 라우터(App.svelte)가 URL을 해석해 올바른 화면을
// 그린다 — 404를 주면 브라우저 기본 에러 페이지가 뜬다.
// serveIndex writes the SPA entry document with a 200 status.
func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// [학습] externalMux의 "/" catch-all에 꽂히는 최상위 핸들러. "/" 요청만 /ui/로
// 302 리다이렉트하고, 그 외 모든 경로는 손대지 않고 apiChain(auditMiddleware +
// authMiddleware를 두른 internalMux)으로 그대로 넘긴다 — 즉 이 함수 자체는 auth 결정에
// 관여하지 않고 단지 방문자 편의를 위한 진입점 하나만 추가한다.
// rootRedirectOr returns a handler that redirects the bare root path to the UI
// and delegates every other path to the existing API chain. This keeps the API
// catch-all (auth + audit) intact while giving visitors a landing page.
func rootRedirectOr(apiChain http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/ui/", http.StatusFound)
			return
		}
		apiChain.ServeHTTP(w, r)
	})
}
