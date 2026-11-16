-------------------------------- MODULE Lease --------------------------------
(***************************************************************************)
(* The worker lease of the task store (lectio_exchange and lectio_reap).   *)
(* Each exchange of a worker renews its lease for Lease seconds. A worker  *)
(* whose lease ran out is taken for dead: a sweep run by another worker    *)
(* removes it and returns its task to the queue, where any worker may      *)
(* claim it under a new token. That sweep runs only for a worker whose own *)
(* last exchange was within a third of the lease, so a stall of the        *)
(* database itself reaps no one.                                           *)
(*                                                                         *)
(* A worker that is running exchanges again within Gap seconds of its      *)
(* last answered exchange: at most Renewal seconds of waiting, and one     *)
(* exchange that takes at most Timeout seconds. The claim checked here is  *)
(* that no running worker is ever taken for dead, so a task is never       *)
(* given to a second worker while the first still runs it. Time is in      *)
(* whole seconds. Lease, Renewal, Timeout and Third are rendered from the  *)
(* Go constants into LeaseConstants.tla by TestLeaseConstants.             *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, LeaseConstants

CONSTANTS Workers, Horizon

None == "none"
Gap == Renewal + Timeout

VARIABLES
    clock,       \* seconds since the start
    seen,        \* worker -> the clock of its last answered exchange
    registered,  \* worker -> TRUE until a sweep removes it
    running,     \* worker -> TRUE while the process runs
    holder,      \* the worker the task is leased to, or None
    token        \* raised by one at every claim

vars == <<clock, seen, registered, running, holder, token>>

Init == /\ clock = 0
        /\ seen = [w \in Workers |-> 0]
        /\ registered = [w \in Workers |-> TRUE]
        /\ running = [w \in Workers |-> TRUE]
        /\ holder = None
        /\ token = 0

\* An exchange renews the lease of a registered worker that runs.
Exchange(w) ==
    /\ running[w] /\ registered[w]
    /\ seen' = [seen EXCEPT ![w] = clock]
    /\ UNCHANGED <<clock, registered, running, holder, token>>

\* A worker's claim leases the task under a new token.
Claim(w) ==
    /\ running[w] /\ registered[w]
    /\ holder = None
    /\ holder' = w
    /\ token' = token + 1
    /\ seen' = [seen EXCEPT ![w] = clock]
    /\ UNCHANGED <<clock, registered, running>>

\* A process stops, and its lease runs out unless it is renewed again.
Crash(w) ==
    /\ running[w]
    /\ running' = [running EXCEPT ![w] = FALSE]
    /\ UNCHANGED <<clock, seen, registered, holder, token>>

\* lectio_reap: a running, registered worker r whose last exchange was within
\* a third of the lease removes every worker whose lease ran out, and returns
\* the task of a removed one to the queue.
Reap(r) ==
    /\ running[r] /\ registered[r]
    /\ clock - seen[r] <= Third
    /\ \E w \in Workers \ {r} :
        /\ registered[w]
        /\ clock - seen[w] >= Lease
        /\ registered' = [registered EXCEPT ![w] = FALSE]
        /\ holder' = IF holder = w THEN None ELSE holder
    /\ UNCHANGED <<clock, seen, running, token>>

\* A second passes. A worker that runs exchanges before Gap seconds have
\* passed since its last answered exchange.
Tick ==
    /\ clock < Horizon
    /\ \A w \in Workers : running[w] /\ registered[w] => clock + 1 - seen[w] <= Gap
    /\ clock' = clock + 1
    /\ UNCHANGED <<seen, registered, running, holder, token>>

\* The model stops at Horizon seconds.
Stop == clock = Horizon /\ UNCHANGED vars

Next == Tick \/ Stop \/ \E w \in Workers : Exchange(w) \/ Claim(w) \/ Crash(w) \/ Reap(w)

Spec == Init /\ [][Next]_vars

\* A worker that runs is never taken for dead, so the task it holds is never
\* given to another worker while it works on it.
Live == \A w \in Workers : running[w] => registered[w]

TypeOK == /\ clock \in 0..Horizon
          /\ \A w \in Workers : seen[w] \in 0..Horizon
          /\ token \in 0..Horizon

\* A worker that waits as long as its lease between exchanges. Lease.slow.cfg
\* uses these.
SlowRenewal == 60
SlowTimeout == 60
=============================================================================
