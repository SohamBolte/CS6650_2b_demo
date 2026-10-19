"""
Load test for the Product API.

Three user classes. The request mix is identical in all of them, so the only
thing that changes between runs is the HTTP client or the think time:

    ProductUser      HttpUser,     python-requests,    realistic think time
    ProductFastUser  FastHttpUser, geventhttpclient,   realistic think time
    StressUser       FastHttpUser, geventhttpclient,   no think time at all

Pick one at the command line, for example:

    locust -f locustfile.py ProductUser     -H http://<IP>:8080
    locust -f locustfile.py ProductFastUser -H http://<IP>:8080
    locust -f locustfile.py StressUser      -H http://<IP>:8080

The task bodies live in module level functions and each class calls them from
its own @task methods. Locust finds tasks with a metaclass that only looks at
the class body, so a shared mixin class would be silently ignored and the run
would fail with "No tasks defined".

The mix is read heavy on purpose. In a real store a product page is viewed far
more often than a product is edited, so eight reads to one write models that
better than an even split.

One task deliberately asks for a product that does not exist. Those 404s are
marked as successes in Locust, because a correct 404 is the server working, not
failing. They still appear as errors in the server's own metrics, which is what
the Part V error panel needs.
"""

import random

from locust import HttpUser, FastHttpUser, task, between, constant

VALID_IDS = [1, 2, 3]

VALID_BODY = {
    "product_id": 1,
    "sku": "ABC-123-XYZ",
    "manufacturer": "Acme Corporation",
    "category_id": 456,
    "weight": 1250,
    "some_other_id": 789,
}


# ///////////////////////// SHARED TASK BODIES /////////////////////////

def get_existing(user):
    pid = random.choice(VALID_IDS)
    # name= groups all three ids into one row in the Locust table. Without it
    # the statistics are split across three separate URLs.
    user.client.get("/products/%d" % pid, name="/products/[id]")


def get_missing(user):
    with user.client.get(
        "/products/9999", name="/products/[missing]", catch_response=True
    ) as response:
        if response.status_code == 404:
            response.success()


def post_details(user):
    with user.client.post(
        "/products/1/details",
        json=VALID_BODY,
        name="/products/[id]/details",
        catch_response=True,
    ) as response:
        if response.status_code == 204:
            response.success()


# ///////////////////////// USER CLASSES /////////////////////////

class ProductUser(HttpUser):
    """The standard client, backed by python-requests."""

    wait_time = between(0.1, 0.5)

    @task(8)
    def t_get_existing(self):
        get_existing(self)

    @task(1)
    def t_get_missing(self):
        get_missing(self)

    @task(1)
    def t_post_details(self):
        post_details(self)


class ProductFastUser(FastHttpUser):
    """The fast client, backed by geventhttpclient. Same mix, same think time."""

    wait_time = between(0.1, 0.5)

    @task(8)
    def t_get_existing(self):
        get_existing(self)

    @task(1)
    def t_get_missing(self):
        get_missing(self)

    @task(1)
    def t_post_details(self):
        post_details(self)


class StressUser(FastHttpUser):
    """
    No think time. Every user sends its next request the instant the previous
    one returns, so throughput is bounded only by latency and by whichever side
    gives out first. This is the class for finding the point where the server
    starts to struggle.
    """

    wait_time = constant(0)

    @task(8)
    def t_get_existing(self):
        get_existing(self)

    @task(1)
    def t_get_missing(self):
        get_missing(self)

    @task(1)
    def t_post_details(self):
        post_details(self)
