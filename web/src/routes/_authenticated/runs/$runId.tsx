import { createFileRoute } from '@tanstack/react-router'
import { RunDetailPage } from '@/features/runs/detail'

function RouteComponent() {
  const { runId } = Route.useParams()
  return <RunDetailPage runId={runId} />
}

export const Route = createFileRoute('/_authenticated/runs/$runId')({
  component: RouteComponent,
})
