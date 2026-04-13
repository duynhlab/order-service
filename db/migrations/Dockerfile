FROM alpine:3.23

ENV FLYWAY_VERSION=11.19.0
ENV FLYWAY_HOME=/opt/flyway/$FLYWAY_VERSION  \
    FLYWAY_PKGS="https://repo1.maven.org/maven2/org/flywaydb/flyway-commandline/${FLYWAY_VERSION}/flyway-commandline-${FLYWAY_VERSION}.tar.gz"

RUN apk upgrade --no-cache && apk add --no-cache \
    openjdk17-jre \
    wget \
    bash \
    ca-certificates

# Download, extract, and setup flyway
RUN wget $FLYWAY_PKGS && \
    mkdir -p $FLYWAY_HOME && \
    tar -xzf flyway-commandline-$FLYWAY_VERSION.tar.gz -C $FLYWAY_HOME --strip-components=1 && \
    rm flyway-commandline-$FLYWAY_VERSION.tar.gz && \
    chmod +x $FLYWAY_HOME/flyway && \
    ln -s $FLYWAY_HOME/flyway /usr/local/bin/flyway

# Copy SQL files to flyway location
COPY sql/ $FLYWAY_HOME/sql/

# Set Flyway locations (build-time configuration)
ENV FLYWAY_LOCATIONS="filesystem:$FLYWAY_HOME/sql"

WORKDIR $FLYWAY_HOME

