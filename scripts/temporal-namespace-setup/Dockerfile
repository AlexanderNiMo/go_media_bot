FROM alpine:3

# bash is needed because the temporal.io CLI installer sources shell snippets
# that use bashisms. ca-certificates gives curl a usable trust store.
RUN apk add --no-cache bash curl ca-certificates

COPY setup-namespace.sh /usr/local/bin/setup-namespace.sh
RUN chmod +x /usr/local/bin/setup-namespace.sh

ENTRYPOINT ["/usr/local/bin/setup-namespace.sh"]