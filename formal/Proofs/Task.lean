import Machines.Task

/-!
Claims about the task table, proved over the table that
`internal/tasks.TaskTable` renders into `Machines/Task.lean`.
-/

namespace Proofs.Task
open Machines.Task Statemachine

/-- A canceled task never runs again: nothing follows `canceled`. -/
theorem canceled_never_runs (s : State) (r : Reaches machine .canceled s) :
    s = .canceled :=
  machine.terminal_stays (by decide) r

/-- A task is leased only from the queue, so a claim is the only way to a
lease. -/
theorem leased_only_from_queued :
    ∀ s ∈ machine.states, State.leased ∈ machine.next s → s = .queued := by
  decide

/-- A task that ended runs again only through the queue: a retry or a second
assemble queues it first. -/
theorem ended_task_runs_only_through_the_queue :
    ∀ s ∈ [State.succeeded, .failed], machine.next s = [State.queued] := by
  decide

/-- Nothing is canceled once it ended: only a queued or leased task is. -/
theorem canceled_only_from_open :
    ∀ s ∈ machine.states, State.canceled ∈ machine.next s →
      s ∈ [State.queued, .leased] := by
  decide

/-- Writers that check the state they write over keep the table whatever
they read and in whatever order they land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Task
