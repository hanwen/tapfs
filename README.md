DEMO
====

Dependencies of a make build; build caching.

Caching
=======

```

# Remove Ccache from patch
export PATH=$(echo $PATH | sed s#/usr/lib64/ccache:##)

# Patch ninja
TAPFS=$PWD
mkdir ~/vc/ninja-build
cd  ~/vc/ninja-build
git clone https://github.com/ninja-build/ninja  
patch -p1  < ${TAPFS}/ninja.patch
cd ninja
./configure
/usr/bin/time ninja

# make a test checkout
git worktree add ../ninja-test

# Start server (other terminal)
(cd $TAPFS && rm -rf ~/tmp/tapfs-db/ && mkdir ~/tmp/tapfs-db && \
    fusermount -z -u /tmp/x ; mkdir -p /tmp/x && go build ./cmd/tapfs-wrapper/ && \
    go build ./cmd/tapfs-server/ && \
    ./tapfs-server --backing ~/vc/ninja-build/ninja-test/  --database ~/tmp/tapfs-db/ /tmp/x )

# Start the build
(cd $HOME/vc/ninja-build/ninja-test  && \
    cd /tmp/x && NINJA_SHELL=$TAPFS/tapfs-wrapper /usr/bin/time ~/vc/ninja-build/ninja/ninja )

# Visualize the build
(cd $TAPFS ; go run cmd/servedeps/main.go  ~/tmp/tapfs-db/deps/ )
google-chrome http://localhost:6710/

# Caching
(cd $HOME/vc/ninja-build/ninja-test && \
    rm -rf build/ ./ninja && \
    cd /tmp/x && NINJA_SHELL=$HOME/go/bin/tapfs-wrapper /usr/bin/time ~/vc/ninja-build/ninja/ninja )

# edit src/ninja.cc ; update --help msg
```


TODO
====

* symlinks.

