import { get } from 'svelte/store'
import { auth, view } from './store.js'

// ===== 학습 노트 (anvil v0.5.x 학습용 주석, 참고 전용 브랜치) =====
// 브리프상 경로는 web/src/api.js였지만 실제 파일 위치는 web/src/lib/api.js다 (다른
// 소스 파일은 모두 web/src/lib/ 아래 있고, web/src/components/*.svelte가 이 모듈의
// apiFetch/apiJSON을 import해 쓴다).
//
// 이 파일이 클라이언트 쪽 토큰 취급 전체를 담당한다: 토큰은 sessionStorage(기본, 탭을
// 닫으면 사라짐 — ephemeral 제품 컨셉에 맞음) 또는 "remember me" 선택 시 localStorage에만
// 저장되고, 번들(JS 코드) 안에는 어떤 비밀도 하드코딩돼 있지 않다 — cmd/goose-daemon/
// ui.go의 "/ui/ 번들은 auth 밖이어도 안전하다"는 전제가 성립하는 근거가 바로 이 파일의
// 설계다. apiFetch가 모든 API 호출에 Authorization 헤더를 주입하고 401을 감지해 로그인
// 화면으로 되돌리는 유일한 지점이라, 개별 컴포넌트는 인증 로직을 전혀 알 필요가 없다.

const TOKEN_KEY = 'ephemera_token'

// [학습] sessionStorage를 localStorage보다 먼저 확인한다 — 같은 탭에서 방금 로그인한
// 토큰(sessionStorage)이 예전에 "remember me"로 저장해 둔 토큰(localStorage)보다
// 우선권을 갖는다는 뜻.
export function loadStoredToken() {
  return sessionStorage.getItem(TOKEN_KEY) || localStorage.getItem(TOKEN_KEY) || null
}

// [학습] remember=false(기본)면 sessionStorage에만 써서 탭을 닫으면 토큰이 사라진다.
// 서버 쪽(cmd/goose-daemon/api.go)은 이 토큰의 발급/만료를 전혀 모른다 — 클라이언트
// 저장 위치 선택은 순수 UX 결정이고 보안 경계는 여전히 서버의 Bearer 비교에 있다.
export function storeToken(token, remember) {
  // sessionStorage by default (cleared on tab close — fits an ephemeral product);
  // localStorage only when the user opts into "remember me".
  if (remember) localStorage.setItem(TOKEN_KEY, token)
  else sessionStorage.setItem(TOKEN_KEY, token)
}

// [학습] 두 저장소 모두 지운다 — remember 여부와 무관하게 로그아웃/401 처리 시
// 어느 쪽에 저장돼 있었는지 신경 쓰지 않고 완전히 지우기 위함(apiFetch의 401 분기,
// Login.svelte의 로그아웃 액션이 호출).
export function clearToken() {
  sessionStorage.removeItem(TOKEN_KEY)
  localStorage.removeItem(TOKEN_KEY)
}

// apiFetch wraps fetch: it injects the bearer header (unless auth is disabled)
// and centralizes the 401 → login redirect so every caller stays simple.
// [학습] 이 함수가 사실상 클라이언트 쪽 authMiddleware다. auth.disabled(서버가 클라이언트
// 0개로 뜬 경우, api.go의 authMiddleware가 즉시 통과시키는 것과 대응)일 땐 헤더 자체를
// 안 붙인다. 401을 받으면 토큰을 지우고 auth store를 초기화한 뒤 view를 login으로
// 되돌려 App.svelte가 자동으로 로그인 화면을 렌더링하게 만든다 — 이 redirect 로직이
// 한 곳에만 있어서 개별 컴포넌트는 401 처리를 신경 쓸 필요가 없다.
export async function apiFetch(path, opts = {}) {
  const a = get(auth)
  const headers = new Headers(opts.headers || {})
  if (!a.disabled && a.token) headers.set('Authorization', 'Bearer ' + a.token)
  if (opts.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')

  const resp = await fetch(path, { ...opts, headers })
  if (resp.status === 401) {
    clearToken()
    auth.update((s) => ({ ...s, token: null }))
    view.set({ name: 'login' })
    throw new Error('unauthorized')
  }
  return resp
}

// apiJSON parses the JSON body and surfaces the daemon's {"error":"..."} shape.
// [학습] 서버가 에러 시 항상 {"error":"..."} 형태로 응답한다는 관례(cmd/goose-daemon의
// writeJSONError 등)에 맞춰 메시지를 뽑아내고, err.status에 HTTP 상태 코드를 얹어
// 던진다 — 호출부가 409(in-use 프로필 삭제 거부 등)처럼 특정 상태 코드를 구분해
// 다르게 처리할 수 있게 하는 관례.
export async function apiJSON(path, opts) {
  const resp = await apiFetch(path, opts)
  const text = await resp.text()
  let data = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = text
    }
  }
  if (!resp.ok) {
    const msg = data && data.error ? data.error : 'HTTP ' + resp.status
    const err = new Error(msg)
    err.status = resp.status // let callers special-case e.g. 409 Conflict
    throw err
  }
  return data
}
