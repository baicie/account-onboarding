# Migration Plan

Schema5 -> 6 adds alert outbox/deliveries. Take encrypted backup, verify manifest/checksum/keyring, then start new binary; old binaries must refuse schema6. Restore rollback uses pre-upgrade backup. Historical old-binary replay remains NOT_RUN.
