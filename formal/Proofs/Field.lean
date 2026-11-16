import Machines.Field

/-!
Claims about the extraction table, proved over the table that
`internal/tasks.FieldTable` renders into `Machines/Field.lean`.
-/

namespace Proofs.Field
open Machines.Field Statemachine

/-- An extraction ends only from pending: a succeeded one never fails and a
failed one never succeeds without being asked again. -/
theorem ends_only_from_pending :
    ∀ s ∈ machine.states, (State.succeeded ∈ machine.next s ∨ State.failed ∈ machine.next s) →
      s = .pending := by
  decide

/-- An extraction that ended goes only to pending, which is being asked
again. -/
theorem ended_goes_only_to_pending :
    ∀ s ∈ [State.succeeded, .failed], machine.next s = [State.pending] := by
  decide

/-- Writers that check the state they write over keep the table whatever
they read and in whatever order they land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Field
