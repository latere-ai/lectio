import Machines.Breaker

/-!
Claims about the breaker table, proved over the table that
`internal/tasks.BreakerTable` renders into `Machines/Breaker.lean`.
-/

namespace Proofs.Breaker
open Machines.Breaker Statemachine

/-- A breaker opens only from closed, and closes only from open. -/
theorem alternates :
    ∀ s ∈ machine.states,
      (State.«open» ∈ machine.next s → s = .closed) ∧
      (State.closed ∈ machine.next s → s = .«open») := by
  decide

/-- Writers that check the state they write over keep the table whatever
they read and in whatever order they land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Breaker
