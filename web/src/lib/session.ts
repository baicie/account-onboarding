import { csrfToken } from '@/lib/api'

// Signing out revokes the server session, then leaves the console for the login page.
export async function logout() {
  try {
    await fetch('/ui/logout', {
      method: 'POST',
      credentials: 'include',
      headers: { 'X-CSRF-Token': csrfToken() },
    })
  } finally {
    window.location.href = '/ui/login'
  }
}
