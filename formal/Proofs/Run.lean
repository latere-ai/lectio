import Machines.Run

/-!
Claims about the figure run table, proved over the table that
`internal/tasks.RunTable` renders into `Machines/Run.lean`.
-/

namespace Proofs.Run
open Machines.Run Statemachine

/-- A run that is running is never replaced: a run starts only over one that
ended. -/
theorem started_only_over_an_ended_run :
    ∀ s ∈ machine.states, State.running ∈ machine.next s →
      s ∈ [State.succeeded, .failed] := by
  decide

/-- A run fails only while it runs. -/
theorem failed_only_from_running :
    ∀ s ∈ machine.states, State.failed ∈ machine.next s → s = .running := by
  decide

/-- Writers that check the state they write over keep the table whatever
they read and in whatever order they land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Run
