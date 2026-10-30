`events.jsonl` is seven lines of a Docker engine's `GET /events`, recorded
on 2026-10-08 from a Docker engine (Docker 29.4.0, API
1.54) with a GET request and nothing else: one `docker run --rm` of a test
image as its lines appeared, create to destroy. Container names are the
engine's random ones.

The tests' volume, image and `exec_start` lines are not recordings. They
are written from the Docker Engine API reference, and say so where they
are used.
