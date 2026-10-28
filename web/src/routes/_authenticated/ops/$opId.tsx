import { createFileRoute } from '@tanstack/react-router'
import { OpsDetailPage } from '@/features/ops/detail'

function RouteComponent() {
  const { opId } = Route.useParams()
  return <OpsDetailPage opId={opId} />
}

export const Route = createFileRoute('/_authenticated/ops/$opId')({
  component: RouteComponent,
})
