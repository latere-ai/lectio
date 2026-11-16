import Machines.Parse

/-!
Claims about the parse table, proved over the table that
`internal/tasks.ParseTable` renders into `Machines/Parse.lean`.
-/

namespace Proofs.Parse
open Machines.Parse Statemachine

/-- A canceled parse is never read again: it is only ever removed. -/
theorem canceled_stays_ended (s : State) (r : Reaches machine .canceled s) :
    s ∈ [State.canceled, .gone] :=
  machine.reaches_closed (by decide) r (by decide)

/-- A removed parse never comes back. -/
theorem gone_stays_gone (s : State) (r : Reaches machine .gone s) : s = .gone :=
  machine.terminal_stays (by decide) r

/-- A parse is queued only when it is submitted: no step leads to `queued`. -/
theorem queued_only_at_submit :
    ∀ s ∈ machine.states, State.queued ∉ machine.next s := by
  decide

/-- A parse runs again only from a queued parse, or from one that ended with
pages to read again: never from a canceled one. -/
theorem running_only_from_queued_or_retried :
    ∀ s ∈ machine.states, State.running ∈ machine.next s →
      s ∈ [State.queued, .succeeded, .failed] := by
  decide

/-- Only a parse that ended is removed. -/
theorem removed_only_when_ended :
    ∀ s ∈ machine.states, State.gone ∈ machine.next s →
      s ∈ [State.succeeded, .failed, .canceled] := by
  decide

/-- Writers that check the state they write over, under the row lock or in
the condition of the statement, keep the table whatever they read and in
whatever order they land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Parse
