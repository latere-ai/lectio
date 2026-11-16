import Machines.Stage

/-!
Claims about the stage an in-memory parse reports, proved over the table that
`internal/tasks.StageTable` renders into `Machines/Stage.lean`.
-/

namespace Proofs.Stage
open Machines.Stage Statemachine

/-- A parse that is done reports no other stage again. -/
theorem done_stays_done (s : State) (r : Reaches machine .done s) : s = .done :=
  machine.terminal_stays (by decide) r

/-- Every parse is assembled before it is done, whichever way it ended. -/
theorem done_only_after_assembling :
    ∀ s ∈ machine.states, State.done ∈ machine.next s → s = .assembling := by
  decide

/-- The stage never goes back: from `reading` a parse is only ever
assembling or done. -/
theorem reading_never_returns (s : State) (r : Reaches machine .reading s) :
    s ∈ [State.reading, .assembling, .done] :=
  machine.reaches_closed (by decide) r (by decide)

end Proofs.Stage
