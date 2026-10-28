// Thin client for the existing JSON API. The console authenticates with the same
// session cookie as the server-rendered pages and sends the CSRF token from
// /ui/api/session on every mutation.
export type Session = {
  authenticated: boolean
  actor?: string
  role?: string
  csrf?: string
  version?: string
  lang?: string
}

let csrf = ''
export const csrfToken = () => csrf

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set('Accept', 'application/json')
  if (init.method && init.method !== 'GET') {
    headers.set('Content-Type', 'application/json')
    headers.set('X-CSRF-Token', csrf)
  }
  const res = await fetch(path, { ...init, headers, credentials: 'include' })
  const text = await res.text()
  const body = text ? JSON.parse(text) : null
  if (!res.ok) {
    throw new ApiError(res.status, body?.error?.message ?? res.statusText)
  }
  return body as T
}

export async function loadSession(): Promise<Session> {
  const s = await request<Session>('/ui/api/session')
  csrf = s.csrf ?? ''
  return s
}

export type Run = {
  id: string
  status: string
  template: string
  mode: string
  current_step?: string
  created_at?: string
  updated_at?: string
}

export const api = {
  runs: (query = '') => request<{ items: Run[]; next_cursor?: string }>('/v1/runs' + (query ? `?${query}` : '')),
  run: (id: string) => request<any>(`/v1/runs/${id}`),
  events: (id: string) => request<{ items: any[] }>(`/v1/runs/${id}/events`),
  createRun: (body: unknown) =>
    request<any>('/v1/runs', {
      method: 'POST',
      body: JSON.stringify(body),
      headers: { 'Idempotency-Key': crypto.randomUUID() },
    }),
  resume: (id: string) => request<any>(`/v1/runs/${id}/resume`, { method: 'POST', body: '{}' }),
  cancel: (id: string) => request<any>(`/v1/runs/${id}/cancel`, { method: 'POST', body: '{}' }),
  outbox: () => request<{ rows?: any[]; items?: any[] }>('/v1/ops/alerts/outbox'),
  outboxItem: (id: string) => request<{ item: any; deliveries: any[] }>(`/v1/ops/alerts/outbox/${id}`),
  dispatch: () => request<any>('/v1/ops/alerts/outbox/dispatch', { method: 'POST', body: '{}' }),
  rearm: (rule: string) =>
    request<any>('/v1/ops/alerts/outbox/rearm', { method: 'POST', body: JSON.stringify({ rule }) }),
  resolveRule: (rule: string) =>
    request<any>('/v1/ops/alerts/outbox/resolve', { method: 'POST', body: JSON.stringify({ rule }) }),
  external: () => request<{ items?: any[]; operations?: any[] }>('/v1/ops/external'),
  externalAudit: (id: string) => request<{ items?: any[] }>(`/v1/ops/external/${id}/audit`),
  resolveExternal: (id: string, body: unknown) =>
    request<any>(`/v1/ops/external/${id}/resolve`, { method: 'POST', body: JSON.stringify(body) }),
}
