# The controller image.
#
# Its build context is controller/ and api/ and nothing else: the component that
# holds a customer's git tokens has no business carrying the fakes, the test
# harness or the agent image in its graph, and a narrow context is how that stops
# being a matter of discipline.
FROM golang:1.26 AS build

ARG VERSION=dev
WORKDIR /src

COPY api/ ./api/
COPY controller/ ./controller/

WORKDIR /src/controller
ENV CGO_ENABLED=0 GOWORK=off
RUN go build -trimpath \
      -ldflags "-s -w -X github.com/automagicops/haliphron/controller/version.Version=${VERSION}" \
      -o /out/haliphron-controller ./cmd/haliphron-controller

# Distroless, non-root, no shell: the controller talks to two APIs and reads one
# Secret, and every other capability is one an attacker inherits for free.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/haliphron-controller /haliphron-controller
USER 65532:65532
ENTRYPOINT ["/haliphron-controller"]
