import Machines.Figure

/-!
Claims about the figure table, proved over the table that
`internal/tasks.FigureTable` renders into `Machines/Figure.lean`.
-/

namespace Proofs.Figure
open Machines.Figure Statemachine

/-- A figure is described, or lost, only while a run holds it pending; a
failed one is also described when the next run starts, if it still holds a
description from an earlier run. -/
theorem succeeded_only_from_pending_or_failed :
    ∀ s ∈ machine.states, State.succeeded ∈ machine.next s →
      s ∈ [State.pending, .failed] := by
  decide

/-- A figure that is described is never lost without a new run taking it:
from `succeeded` the only successor is `pending`. -/
theorem described_is_lost_only_by_a_new_run :
    machine.next .succeeded = [State.pending] := by
  decide

/-- Writers that check the state they write over keep the table whatever
they read and in whatever order they land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Figure
