import { createFileRoute } from '@tanstack/react-router'
import { AuthenticatedLayout } from '@/components/layout/authenticated-layout'
import { loadSession } from '@/lib/api'

// The console shares the session cookie with the Go service. When the bootstrap fails the
// operator is sent to the login page instead of seeing an empty shell.
export const Route = createFileRoute('/_authenticated')({
  beforeLoad: async () => {
    try {
      const session = await loadSession()
      return { session }
    } catch {
      if (typeof window !== 'undefined') {
        window.location.assign('/ui/login')
      }
      // Never resolve: the navigation above takes over.
      return await new Promise<never>(() => {})
    }
  },
  component: AuthenticatedLayout,
})
