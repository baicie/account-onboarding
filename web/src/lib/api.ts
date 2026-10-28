export type Session = { authenticated: boolean; actor?: string; role?: string; csrf?: string; version?: string; lang?: string }

let csrf = ''

export function csrfToken(): string { return csrf }

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set('Accept', 'application/json')
  if (init.method && init.method !== 'GET') {
    headers.set('X-CSRF-Token', csrf)
    headers.set('Content-Type', 'application/json')
  }
  const res = await fetch(path, { ...init, headers, credentials: 'include' })
  if (res.status === 401) {
    window.location.href = '/ui/login'
    throw new Error('unauthorized')
  }
  const text = await res.text()
  const body = text ? JSON.parse(text) : null
  if (!res.ok) {
    const msg = body?.error?.message ?? res.statusText
    throw new Error(msg)
  }
  return body as T
}

export async function loadSession(): Promise<Session> {
  const s = await request<Session>('/ui/api/session')
  csrf = s.csrf ?? ''
  return s
}

export const api = {
  runs: () => request<any>('/v1/runs'),
  run: (id: string) => request<any>(`/v1/runs/${id}`),
  events: (id: string) => request<any>(`/v1/runs/${id}/events`),
  createRun: (body: unknown) => request<any>('/v1/runs', { method: 'POST', body: JSON.stringify(body) }),
  resume: (id: string) => request<any>(`/v1/runs/${id}/resume`, { method: 'POST', body: '{}' }),
  cancel: (id: string) => request<any>(`/v1/runs/${id}/cancel`, { method: 'POST', body: '{}' }),
  providers: () => request<any>('/v1/providers'),
  outbox: () => request<any>('/v1/ops/alerts/outbox'),
  dispatch: () => request<any>('/v1/ops/alerts/outbox/dispatch', { method: 'POST', body: '{}' }),
  external: () => request<any>('/v1/ops/external'),
  resolveExternal: (id: string, body: unknown) => request<any>(`/v1/ops/external/${id}/resolve`, { method: 'POST', body: JSON.stringify(body) }),
}
