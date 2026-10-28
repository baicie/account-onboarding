import { createFileRoute } from '@tanstack/react-router'
import { AlertDetailPage } from '@/features/alerts/detail'

function RouteComponent() {
  const { alertId } = Route.useParams()
  return <AlertDetailPage alertId={alertId} />
}

export const Route = createFileRoute('/_authenticated/alerts/$alertId')({
  component: RouteComponent,
})
