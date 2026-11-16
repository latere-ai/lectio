import Machines.File

/-!
Claims about the file table, proved over the table that
`internal/tasks.FileTable` renders into `Machines/File.lean`.
-/

namespace Proofs.File
open Machines.File Statemachine

/-- A file whose delete began never comes back: it is only ever removed. -/
theorem deleting_never_returns (s : State) (r : Reaches machine .deleting s) :
    s ∈ [State.deleting, .gone] :=
  machine.reaches_closed (by decide) r (by decide)

/-- A removed file never comes back. -/
theorem gone_stays_gone (s : State) (r : Reaches machine .gone s) : s = .gone :=
  machine.terminal_stays (by decide) r

/-- A file's row is removed only after its delete began, so no object is
left that no row names. -/
theorem removed_only_after_delete_began :
    ∀ s ∈ machine.states, State.gone ∈ machine.next s → s = .deleting := by
  decide

end Proofs.File
