import { useDeleteRun } from '../api/hooks'
import { Dialog, ErrorBanner } from './ui'

/**
 * The confirmation before a run is deleted, shared by the list and the run's
 * own page. Only a finished run is offered: the backend refuses one that has
 * not ended, and the way to delete a live run is to cancel it first.
 */
export function DeleteRunDialog({
  runId,
  onClose,
  onDeleted,
}: {
  runId: string
  onClose: () => void
  onDeleted?: () => void
}) {
  const remove = useDeleteRun()
  return (
    <Dialog
      title="Delete this run"
      onClose={onClose}
      footer={
        <>
          <button className="ghost" onClick={onClose}>
            Keep it
          </button>
          <button
            className="danger"
            disabled={remove.isPending}
            onClick={() =>
              remove.mutate(runId, {
                onSuccess: () => {
                  onClose()
                  onDeleted?.()
                },
              })
            }
          >
            {remove.isPending ? 'Deleting…' : 'Delete run'}
          </button>
        </>
      }
    >
      <ErrorBanner error={remove.error} what="delete the run" />
      <p className="mono small" style={{ margin: 0 }}>
        {runId}
      </p>
      <p className="small muted" style={{ margin: 0 }}>
        The run, its attempts, its logs and its result are deleted for good. Runs it started are
        kept, without their link back to it. The audit log keeps a record that it existed and what it
        cost. Deleting needs an admin token.
      </p>
    </Dialog>
  )
}
