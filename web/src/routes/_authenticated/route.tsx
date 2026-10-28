import { createFileRoute, redirect } from '@tanstack/react-router'
import { AuthenticatedLayout } from '@/components/layout/authenticated-layout'
import { loadSession } from '@/lib/api'

// The console shares the session cookie with the server-rendered pages. If the bootstrap
// fails the operator is sent to the login page instead of seeing an empty shell.
export const Route = createFileRoute('/_authenticated')({
  beforeLoad: async () => {
    try {
      const session = await loadSession()
      return { session }
    } catch {
      throw redirect({ to: '/', reloadDocument: true, href: '/ui/login' } as never)
    }
  },
  component: AuthenticatedLayout,
})
