# ADR 0007: Consistent hashing with bounded loads

Status: accepted.

## Context

The gateway has to map a key to a cache node.
Two properties are wanted and they pull against each other.

Stickiness: the same key should reach the same node, or the cluster's effective capacity collapses to one node's worth of distinct objects.

Balance: no node should take a disproportionate share of the load.

Plain consistent hashing gives the first and says nothing about the second.
One viral key, or an unlucky hash distribution, and a single node saturates while its peers idle.
Modulo hashing gives balance but moves nearly the whole keyspace when the node count changes.

## Decision

Consistent Hashing with Bounded Loads: Mirrokni, Thorup and Zadimoghaddam, 2016, the algorithm behind Google Cloud Load Balancing.

Each node gets `replicas` virtual nodes on the ring, 128 by default.
Each node has a capacity of `factor` times the current average load, 1.25 by default, so a node may carry 25% more than its fair share.
A key landing on a full node walks forward around the ring to the next node with room.

The gateway charges a node's load counter before forwarding and releases it on the way out, so "current load" means in-flight requests rather than a historical average.

## Consequences

Keys stay sticky while there is slack and spill predictably when there is not.
A hot key becomes a small amount of extra cache duplication rather than an overloaded node, which is the property being bought.

Adding or removing a node still moves only about 1/n of the keyspace.

The load counters are gateway-process memory and only meaningful live, so they do not survive a restart and are not intended to.

The bound is a knob with a real trade-off, documented in [05-operations.md](../05-operations.md): a lower `factor` balances harder and duplicates more, a higher one is stickier and tolerates more skew.

Spill is invisible to correctness because a cache node is authoritative for nothing.
A key served from a second node is a miss that becomes a fetch, not a wrong answer.
This is what makes bounded loads safe here and would not be true for a sharded database.

## What was given up

**Plain consistent hashing**, simpler and already sufficient for a uniform workload.
Rejected because the workload under test is Zipfian with `s = 1.1`, which is specifically the case where a small number of keys carry a large share of requests.
Choosing a scheme that ignores skew while benchmarking against skew would be measuring around the problem.

**Modulo or round-robin hashing.**
Rejected because either destroys stickiness, and without stickiness the three-node cluster caches each object up to three times and the hit-ratio comparison stops being about the policy.

**Centralised load-aware routing**, where the gateway tracks per-node latency and picks the least loaded.
Rejected because it abandons stickiness as a first-class property and adds a control loop to tune; bounded loads gets most of the benefit from one constant.
