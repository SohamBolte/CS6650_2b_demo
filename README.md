# CS6650 Homework 5: Product API on ECS, load tested and instrumented

Soham Santra. Forked from `RuidiH/CS6650_2b_demo` and adapted to deploy a
Product API implemented against the course OpenAPI specification.

---

## Contents

| Path | What is there |
|---|---|
| `src/product-service.go` | The Product API. Gin handlers, in-memory store, Prometheus metrics, JSON request logging |
| `src/Dockerfile` | Multi-stage build, cross compiled to amd64 for Fargate |
| `terraform/` | Infrastructure as code: ECR, ECS/Fargate, CloudWatch logs, networking |
| `locust/locustfile.py` | Load test, three user classes |
| `locust/*.csv` | Raw results from the two controlled headless runs |
| `observability/` | Local Prometheus and Grafana stack, with a provisioned RED dashboard |
| `docs/api.yaml` | The specification this API implements |
| `docs/request-logs-excerpt.txt` | Structured log sample from the Part V run |
| `screenshots/` | Evidence for every part |

---

## Quick start

### Deploy the infrastructure

```bash
# 1. Credentials from the AWS Learner Lab
aws configure
aws configure set aws_session_token <YOUR-TEMP-SESSION-TOKEN>
aws configure set region us-east-1
aws sts get-caller-identity          # must succeed before continuing

# 2. Deploy
cd terraform
terraform init
terraform apply
```

Terraform builds the Docker image, pushes it to ECR, and starts the service.
No manual `docker build`, `docker push` or `aws ecr get-login-password` needed.

### Find the public IP

A Fargate task's public IP is assigned at launch and is three levels deep, so
it has to be looked up rather than remembered. From `terraform/`:

```bash
IP=$(aws ec2 describe-network-interfaces \
  --network-interface-ids $(
    aws ecs describe-tasks \
      --cluster $(terraform output -raw ecs_cluster_name) \
      --tasks $(aws ecs list-tasks \
        --cluster $(terraform output -raw ecs_cluster_name) \
        --service-name $(terraform output -raw ecs_service_name) \
        --query 'taskArns[0]' --output text) \
      --query "tasks[0].attachments[0].details[?name=='networkInterfaceId'].value" \
      --output text) \
  --query 'NetworkInterfaces[0].Association.PublicIp' --output text)
echo $IP
```

```bash
curl http://$IP:8080/health
```

### Run it locally instead

```bash
cd src
go mod tidy
go run product-service.go          # listens on :8080
```

Or in Docker:

```bash
cd src
docker build -t product-service .
docker run -p 8080:8080 product-service
```

### Tear down

```bash
cd terraform
terraform destroy
```

---

## Part II: The Product API

### Two readings of the specification, and which one this implements

`api.yaml` has a gap worth naming. `POST /products/{productId}/details` can
return **404 Product not found**, but there is no endpoint anywhere in the
Product section that *creates* a product. Taken literally, nothing can ever
exist, and every POST returns 404 forever.

Two readings are possible:

- **Upsert.** POST creates the product if absent. Natural, but 404 becomes
  unreachable, and the assignment requires triggering every response code.
- **Update only.** POST modifies products that already exist, and the store is
  seeded at startup.

**This implementation uses update-only**, because it is the only reading under
which every documented response code can actually be produced. Three products
are seeded with IDs 1, 2 and 3.

### Why every field is a pointer

In the `Product` schema, `weight` has `minimum: 0` while every other numeric
field has `minimum: 1`. A weight of zero grams is therefore valid input.

In Go, `0` is also the zero value of `int32`. Using plain `int32` with Gin's
`binding:"required"` would make Gin treat a legitimate `"weight": 0` as a
missing field and reject the request with a 400. Every field is a pointer
instead, so `nil` means absent and `0` means zero. The test suite includes a
`weight: 0` case specifically to prove this works.

### Where the status codes come from

`GET /products/{productId}` defines only 200, 404 and 500, with no 400. So a
request for `/products/abc` has no 400 available and is reported as 404.
`POST .../details` does define a 400, so the same malformed ID is a 400 there.
The same mistake maps to two different codes because the contract says so, not
because of preference.

The body's `product_id` must match the path's `{productId}`. The spec does not
say either way, but allowing them to disagree would let a write to one ID carry
a different ID inside it.

### Triggering 500

A 500 means the server broke, so no well-formed request should cause one. A
deliberate test hook is used: a `sku` of `FORCE_500` on either endpoint returns
500. This is a test affordance, not organic behaviour.

### Every response code

`screenshots/part2-local-curl.png` (via `go run`) and
`screenshots/part2-docker-curl.png` (inside the container) show all of these.
`screenshots/part3-deployed-curl.png` shows the same suite against the
deployed service.

| Code | Endpoint | How to trigger |
|---|---|---|
| 200 | GET | `curl http://$IP:8080/products/1` |
| 404 | GET | `curl http://$IP:8080/products/9999` |
| 404 | GET | `curl http://$IP:8080/products/abc` (no 400 defined on GET) |
| 204 | POST | valid body, existing ID |
| 400 | POST | malformed JSON |
| 400 | POST | missing required field |
| 400 | POST | `category_id: 0`, below its minimum |
| 400 | POST | body `product_id` differs from the path |
| 204 | POST | `weight: 0`, legal per the spec |
| 404 | POST | ID 9999, which does not exist |
| 500 | POST | `sku: "FORCE_500"` |

```bash
# 200
curl -i http://$IP:8080/products/1

# 404, no such product
curl -i http://$IP:8080/products/9999

# 404, unusable id (GET defines no 400)
curl -i http://$IP:8080/products/abc

# 204, valid update
curl -i -X POST http://$IP:8080/products/1/details \
  -H "Content-Type: application/json" \
  -d '{"product_id":1,"sku":"ABC-123-XYZ","manufacturer":"Acme Corporation","category_id":456,"weight":1250,"some_other_id":789}'

# 204, weight of 0 is legal
curl -i -X POST http://$IP:8080/products/2/details \
  -H "Content-Type: application/json" \
  -d '{"product_id":2,"sku":"DEF-456-UVW","manufacturer":"Globex","category_id":457,"weight":0,"some_other_id":790}'

# 400, malformed JSON
curl -i -X POST http://$IP:8080/products/1/details \
  -H "Content-Type: application/json" -d '{"product_id":1,'

# 400, missing required field
curl -i -X POST http://$IP:8080/products/1/details \
  -H "Content-Type: application/json" \
  -d '{"product_id":1,"manufacturer":"Acme","category_id":456,"weight":1250,"some_other_id":789}'

# 400, category_id below minimum
curl -i -X POST http://$IP:8080/products/1/details \
  -H "Content-Type: application/json" \
  -d '{"product_id":1,"sku":"ABC","manufacturer":"Acme","category_id":0,"weight":1250,"some_other_id":789}'

# 400, body id does not match path id
curl -i -X POST http://$IP:8080/products/1/details \
  -H "Content-Type: application/json" \
  -d '{"product_id":2,"sku":"ABC","manufacturer":"Acme","category_id":456,"weight":1250,"some_other_id":789}'

# 404, no such product
curl -i -X POST http://$IP:8080/products/9999/details \
  -H "Content-Type: application/json" \
  -d '{"product_id":9999,"sku":"ABC","manufacturer":"Acme","category_id":456,"weight":1250,"some_other_id":789}'

# 500, deliberate test hook
curl -i -X POST http://$IP:8080/products/1/details \
  -H "Content-Type: application/json" \
  -d '{"product_id":1,"sku":"FORCE_500","manufacturer":"Acme","category_id":456,"weight":1250,"some_other_id":789}'
```

### Data structure

A `map[int32]Product` behind a `sync.RWMutex`.

The map gives O(1) lookup, which suits a read-dominated workload. The RWMutex
is required because every Gin handler runs in its own goroutine; without it,
concurrent requests are a data race on the map, which in Go is not a wrong
answer but an uncatchable `fatal error: concurrent map writes`.

RWMutex rather than Mutex because reads vastly outnumber writes here, so
unlimited concurrent readers is the right trade. Worth noting that in a
previous homework's write-heavy benchmark, RWMutex lost to a plain Mutex
(12.07ms vs 8.48ms). That result does not contradict this choice. It says
RWMutex is bad at writes, and this workload is the opposite shape.

---

## Part III: Terraform

### What each file does

| File | Role |
|---|---|
| `provider.tf` | Declares the AWS and Docker providers. Fetches an ECR auth token as a data source and hands it to the Docker provider, which replaces the manual `docker login` step |
| `variables.tf` | Every value intended to be changed: region, repository name, service name, container port, task count, log retention |
| `main.tf` | Wires the four modules together, then builds and pushes the image |
| `outputs.tf` | Surfaces the cluster and service names, which the IP lookup depends on |
| `modules/network/` | Looks up the default VPC and its subnets, creates a security group opening the container port |
| `modules/ecr/` | Creates the container registry |
| `modules/logging/` | Creates the CloudWatch log group at `/ecs/<service_name>` |
| `modules/ecs/` | Cluster, Fargate task definition, and the ECS **service** that keeps a task running |

The dependency order is never declared anywhere. `main.tf` passes
`"${module.ecr.repository_url}:latest"` into the ECS module, and that reference
alone is what tells Terraform to build ECR first. The graph is inferred from
references, not written down.

`data "aws_iam_role" "lab_role"` is a lookup, not a resource. LabRole already
exists; Terraform only reads its ARN. It is passed as both the execution role
(used by ECS to pull the image and write logs *before* the container starts)
and the task role (used by the application itself *while* running).

### What was changed from the starter

1. **`src/main.go` deleted**, replaced with `product-service.go`, plus its own
   `go.mod` and `go.sum`.
2. **`src/Dockerfile` replaced**, with one important difference described below.
3. **`variables.tf` renamed and re-regioned**: `aws_region` to `us-east-1`,
   `ecr_repository_name` to `product-service`, `service_name` to `product-api`.
   These were changed before the first apply, because changing `service_name`
   afterwards forces Terraform to destroy and recreate the cluster.
4. **A missing dependency was added.** `module.ecs` referenced
   `module.ecr.repository_url`, so it waited for the *repository*, but nothing
   made it wait for `docker_registry_image.app`, the actual *push*. Terraform
   could legally start the service before the image existed. The service
   retries until the pull succeeds, so it self-heals, but the gap is real:

   ```hcl
   module "ecs" {
     source     = "./modules/ecs"
     depends_on = [docker_registry_image.app]
     ...
   }
   ```

### The platform problem

The development machine is an Apple Silicon Mac (arm64). Fargate runs amd64. An
arm64 image fails to start on Fargate with an exec format error.

In previous assignments this was solved with `docker build --platform
linux/amd64` on the command line, but here Terraform runs the build, so there
is no command line to pass the flag to. It has to live in the Dockerfile:

```dockerfile
FROM golang:1.27 AS builder
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o product-service .

FROM --platform=linux/amd64 alpine:latest
```

The builder stage runs natively at full speed and *cross compiles* to amd64,
which is far faster than emulating an amd64 builder. The final stage is pinned
so the resulting image is tagged amd64. Both halves are needed: an amd64 binary
inside an arm64 image would still fail.

Verify before applying:

```bash
cd src && docker build -t check . && docker image inspect check --format '{{.Architecture}}'
# must print: amd64
```

### Notes on the design

The starter deploys an ECS **Service**, not a bare task. A service is a
supervisor: `desired_count = 1` means ECS restarts the task if it dies.

There is no load balancer, which is why finding the public IP takes a four-step
command and why the IP changes whenever the task restarts. An ALB would give
clients one stable address. It was left out as being beyond this assignment,
but it is the obvious next change.

Six subnets are passed to the service, one per availability zone. At
`desired_count = 1` that spread is unused, but raising the count would place
tasks across zones so that losing one datacentre would not take the service
down.

---

## Part IV: Load testing

All runs target the deployed service at `http://54.173.204.114:8080` from a
MacBook Air in Boston. `locust/locustfile.py` defines three user classes with
an identical request mix, so only one variable changes per run.

### Test design

**Request mix, 8:1:1.** Eight reads of an existing product, one read of a
missing product, one write. Product pages are viewed far more often than
products are edited, so a read-heavy mix models a real store better than an
even split. The missing-product task exists to exercise the error path, and
its 404s are marked as successes in Locust because a correct 404 is the server
working, not failing. They still register as errors in the server's own
metrics, which is what the Part V error panel needs.

**Grouped names.** Requests use `name="/products/[id]"` so that three distinct
product IDs collapse into one row rather than splitting the statistics.

**Think time.** `between(0.1, 0.5)` for the realistic runs. `constant(0)` for
`StressUser`, which is what makes the stress test possible at all. See below.

### Results

| Run | Class | Users | Spawn | Requests | RPS | Median | p95 | p99 | Avg | Min | Max | Fails |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | ProductUser | 10 | 10 | 3,507 | 29.1 | 29 | 110 | 120 | 41.82 | 18 | 260 | 0 |
| 2 | ProductUser | 50 | 10 | 17,078 | 143.6 | 33 | 110 | 120 | 44.91 | 18 | 318 | 0 |
| 3 | ProductFastUser | 50 | 10 | 17,140 | 145.1 | 30 | 110 | 120 | 42.54 | 18 | 291 | 0 |
| 4 | StressUser | 200 | 20 | 293,563 | 3,337 | 45 | 130 | 160 | 56.83 | 21 | 311 | 0 |
| 5 | StressUser | 500 | 50 | 513,526 | 4,294 | 96 | 190 | 240 | 104.82 | 23 | 412 | 0 |

All times in milliseconds. Zero failures in every run, including 513,526
requests at 500 concurrent users.

Runs 2 and 3 were also executed headless with `--csv` for exactly two minutes
each, to make the client comparison strictly controlled. Those raw numbers are
in `locust/run2_http_stats.csv` and `locust/run3_fast_stats.csv` and agree with
the UI runs to within 2%.

### The server was idle until think time was removed

Runs 1 and 2 scale perfectly linearly: 5x the users gives 4.9x the throughput,
and latency does not move (median 29 to 33ms, p95 110 to 110ms). That is the
signature of a server doing nothing. Every request is waiting on the network.

The throughput was simply the load generator's own arithmetic:
50 users ÷ (0.3s average wait + 0.03s latency) ≈ 150 RPS, measured 143.6.

**The client, not the server, was the bottleneck**, and no amount of extra
users with think time would have changed that. This is why `StressUser` exists
with `wait_time = constant(0)`. Removing think time alone took throughput from
143 RPS to 3,337 RPS, a 23x increase against the same server.

### Where the server started to struggle

Comparing runs 4 and 5:

| | 200 users | 500 users | ratio |
|---|---|---|---|
| RPS | 3,337 | 4,294 | **1.29x** |
| Median | 45ms | 96ms | **2.13x** |
| p95 | 130ms | 190ms | 1.46x |
| Min | 21ms | 23ms | 1.10x |

**2.5x the load bought 1.29x the throughput and cost 2.13x the latency.** Past
roughly 200 concurrent users, additional load buys queue rather than work.
Throughput per user fell from 16.7 to 8.5 requests per second, almost exactly
halved. The ceiling is around 4,300 RPS.

Note that `min` rose too, from 21ms to 23ms. Even the luckiest request got
slower, meaning the queue was never empty.

A single run's charts look flat rather than climbing, which is correct: a
closed-loop test with fixed users and no think time settles into equilibrium
where throughput × latency = user count. **The knee is visible between runs,
not within one.**

### Proving which side saturated

A measured slowdown means nothing until you know which end caused it. Both ends
were checked during run 5.

**The load generator was not the limit.** `top` during the run showed the
Locust process at 37.4% of one core and the Mac 87% idle.

**The server was.** `screenshots/run5-cloudwatch-cpu.png`:

| Timestamp (EDT) | Avg CPU | Max CPU |
|---|---|---|
| 20:49 | 0.005% | 0.01% |
| 20:50 | 0.02% | 0.07% |
| 20:51 | 33.15% | 99.46% |
| **20:52** | **99.49%** | **99.83%** |
| 20:53 | 66.10% | 99.33% |
| 20:54 | 0.0% | 0.0% |

The task is allocated `cpu = "256"`, a quarter of one vCPU. It sat at **99.49%
average utilisation** for the entire middle minute of the run and returned to
zero immediately after. The timestamps line up exactly with the Locust charts.

At ~4,300 RPS on a quarter core, each request costs roughly 60 microseconds of
CPU, which matches the per-request durations observed in the local container
logs (0.015 to 0.17ms).

### HttpUser vs FastHttpUser

Runs 2 and 3, identical load, only the HTTP client changed:

| | HttpUser | FastHttpUser | Difference |
|---|---|---|---|
| RPS | 143.6 | 145.1 | +1% |
| Median | 33ms | 30ms | **-3ms** |
| Average | 44.91ms | 42.54ms | **-2.4ms** |
| p95 | 110ms | 110ms | none |
| p99 | 120ms | 120ms | none |
| Min | 18ms | 18ms | none |

**Throughput identical, about 5% off median and average, percentiles unchanged.**

`HttpUser` is backed by python-requests, which allocates more per request and
contends on the GIL. `FastHttpUser` uses geventhttpclient, which is lighter.
The 2 to 3ms difference is the real cost of that client overhead.

The interesting part is the comparison with the same experiment on localhost in
Homework 3, where swapping to FastHttpUser cut p99 by 39% and max by 71%.
Nothing about the clients changed between the two experiments. What changed is
what else was in the measurement.

On localhost the round trip was effectively zero, so the client's 2 to 3ms was
a large share of the total and removing it was clearly visible. Here the round
trip to us-east-1 is about 30ms, so that same 2 to 3ms is roughly 5% of the
total and optimising it away is barely detectable. **Amdahl's law: you cannot
speed up a system by more than the share of it you are speeding up.** The same
change looks like a large win in one environment and noise in another, and the
law predicts both.

One place the difference did show visibly: run 2's p95 overshot to 140ms during
the ramp before settling at 110, while run 3 never overshot. Connection setup
under a burst of new users is exactly where the lighter client helps.

### Tradeoffs: which operations dominate, and what that means

Reads dominate overwhelmingly in a product catalogue. That justifies two
choices already made and suggests two more:

- **Map over slice.** O(1) by ID rather than a linear scan.
- **RWMutex over Mutex.** Unlimited concurrent readers; writers serialise.
- **Caching would help enormously** and is the obvious next step, since product
  data changes rarely and tolerates staleness.
- **Scaling out does not work as written.** `ecs_count = 2` would double CPU
  capacity, and reads would scale fine, but each task has its own in-memory
  map. A POST to task 1 would be invisible to task 2. This is precisely the
  state-divergence failure from Homework 2 Part IV, where two instances of the
  albums API returned different data after a write to only one of them.

The CPU ceiling is fixable with one variable. The state problem is not; it
needs shared storage. **The load test found the easy limit, and reasoning about
the fix found the real one.**

---

## Part V: Observability

The server exposes `/metrics` in Prometheus format and emits one structured
JSON log line per request. Both come from the same middleware and the same
measurement, so the dashboard and the logs cannot disagree.

### Running the stack

```bash
cd observability
docker compose up --build
```

- App: `http://localhost:8080`
- Prometheus: `http://localhost:9090` (check `/targets` says UP)
- Grafana: `http://localhost:3000`. The dashboard "Product API - RED" is
  provisioned automatically, no login

Kept local deliberately, as the assignment directs. Prometheus service
discovery and persistent storage on ECS is a separate project.

### The three signals

| Signal | Metric | Query |
|---|---|---|
| Rate | `http_requests_total` | `sum by (route) (rate(http_requests_total[1m]))` |
| Errors | `http_request_errors_total` | `100 * sum(rate(http_request_errors_total[1m])) / sum(rate(http_requests_total[1m]))` |
| Duration | `http_request_duration_seconds` | `histogram_quantile(0.95, sum by (le, route) (rate(http_request_duration_seconds_bucket[1m])))` |

Two implementation details that matter:

**Labelled by route, not path.** `c.FullPath()` gives `/products/:productId`,
so a million distinct product IDs collapse into one time series. Labelling by
raw path would create a new series per ID and eventually bring Prometheus down.
This is the standard way people break a metrics backend.

**Custom histogram buckets.** Prometheus' default buckets start at 5ms. These
handlers finish in tens of microseconds, so every request would land in the
first bucket and every quantile would be meaningless. The buckets used are
`ExponentialBuckets(0.0001, 2, 14)`, running from 100µs to about 0.8s.

### The dashboard

`screenshots/part5-grafana-red.png` shows idle, then a 50-user 3-minute Locust
run, then idle again: flat, spike, flat on all three signals.

Three things worth reading in it:

**Rate reads ~145 req/s, and Locust measured 143.6.** Two independent
measurements, one from the client and one from inside the server, agreeing.
That cross-check is the main reason to instrument at all.

**Errors sit at a flat 10%, all of them 404.** That is by design: one request
in ten asks for a missing product. The lesson is that **10% errors is this
service's healthy baseline**, so alerting on "any errors" would be useless.
Alerts belong on deviation from normal, not on the presence of errors.

**The slowest endpoint is `/metrics` itself.** The product routes sit on the
floor at sub-millisecond, while p95 of `/metrics` runs around 12ms idle and
jumps to **37ms under load**, roughly 3x. Under load the histogram fills more
buckets and more label combinations, so each scrape has more series to
serialise. Observing the system costs more than running it. This was not
planned, and it is a good argument for keeping metric cardinality low.

### The same signals in logs

`docs/request-logs-excerpt.txt`:

```json
{"time":"2026-10-19T00:19:41.493696096Z","method":"GET","route":"/products/:productId","path":"/products/1","status":200,"duration_ms":0.017,"client_ip":"192.168.65.1"}
{"time":"2026-10-19T00:19:41.737771929Z","method":"GET","route":"/products/:productId","path":"/products/9999","status":404,"duration_ms":0.168,"client_ip":"192.168.65.1"}
{"time":"2026-10-19T00:19:41.533715471Z","method":"POST","route":"/products/:productId/details","path":"/products/1/details","status":204,"duration_ms":0.145,"client_ip":"192.168.65.1"}
```

Each line carries route, status and duration: the same three signals, one
request at a time. Both `route` and `path` are recorded, because the template
is what you aggregate on and the concrete path is what you debug with.

The excerpt also caught something unplanned. The first ten lines of the run
range from 2.9 to 23.4ms, all within 5ms of each other, and from line eleven
onward everything sits near 0.1ms, roughly a **200x drop**. That is the cold
start: the first wave of requests against a server that has never served one.
The first measurement of anything is never the real one.

Also produced in the deployed environment, via the CloudWatch log group
`/ecs/product-api` created by the logging module:

```bash
aws logs tail /ecs/product-api --since 10m
```

### Dashboard or logs?

**The dashboard answers "is something wrong, and since when."** It is
aggregate, continuous, and already running, so it is what you look at first and
what an alert points you to. It cannot tell you *which* request failed.

**The logs answer "what exactly happened to this request."** One line per
request, with the concrete path, so once the dashboard says p95 doubled at
20:52, the logs are where you find out whether it was one slow route, one
client, or everything at once.

In short: the dashboard to notice and to scope, the logs to diagnose. The
dashboard is also the only one of the two that is useful *before* you know
something is wrong.

---

## Design question: a scalable backend for the full API

The honest starting point is what is here: one process, an in-memory map, one
Fargate task. It saturates at ~4,300 RPS on a quarter vCPU and loses all its
data whenever the task restarts.

**Split along the tags already in the spec.** `api.yaml` groups endpoints into
Products, Shopping Cart, Warehouse and Payments. Those are four services, and
the specification drew the boundaries. They have genuinely different needs,
which is the argument for separating them rather than a preference for
microservices.

- **Products**: read-heavy, rarely written, tolerates staleness. Cache
  aggressively, CDN in front, many stateless replicas. This carries most of the
  traffic and is the easy part.
- **Shopping Cart**: per-user state with no cross-user contention. Shard by
  `customer_id`; each shard is independent.
- **Warehouse**: the hard one. `/warehouse/reserve` is the only place in the
  file where two users genuinely contend for the same row, and two people
  reserving the last unit must not both succeed. That needs a real transaction
  against a single authority. Do not shard the thing that must be serialised.
- **Payments**: must be idempotent. A timeout followed by a client retry must
  not charge the card twice. Every request carries an idempotency key and the
  server remembers which keys it has seen.

**Checkout is a distributed transaction, and you cannot have one.** It touches
cart, warehouse and payments. If payment succeeds and shipping fails, a charge
cannot be un-made. The answer is a saga: a sequence of steps each with a
compensating action (refund rather than un-charge). The system is briefly
inconsistent and converges.

**What replaces the map is a database**, and the reason is not throughput. The
real problem with the current design is that state lives inside the process, so
two replicas means two different answers to the same question.

---

## Design question: declarative vs imperative

**Imperative is a recipe. Declarative is an order.**

Imperative: "create a repository, log into it, build the image, push it, create
a cluster, register a task definition, start a service." You list the steps, in
order, and you are responsible for the order being correct.

Declarative: "there should be a repository called `product-service`, a cluster
called `product-api-cluster`, and one running task." You describe the end
state; Terraform works out the steps.

Three consequences, each with evidence in this repo:

**The order is never written down.** The ECS module is created after ECR purely
because `main.tf` references `module.ecr.repository_url`. Terraform builds a
dependency graph from references. The one place that graph was *missing* an
edge, the image push, had to be fixed with an explicit `depends_on`, which is
itself evidence that the rest was inferred.

**It is idempotent.** `terraform apply` run twice does nothing the second time,
because the desired state already matches reality. An imperative script run
twice creates two of everything. This is the same property that makes a retried
network request safe, and it is why declarative tooling survives being
interrupted halfway.

**It can tell you what will change before changing it.** `terraform plan`
printed "8 to add, 0 to change, 0 to destroy" before anything existed. A shell
script cannot offer that, because the only way to know what a sequence of
commands will do is to run it.

The cost is real too. When you want something the tool does not model, you
fight it. And `terraform destroy` will cheerfully delete everything, because
"nothing should exist" is also a valid declaration.
