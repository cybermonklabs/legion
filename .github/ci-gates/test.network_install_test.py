# The network_install_test filter's gate: what job test reads.
GATE = harness("network_install_test", "Test the bounds on network installs and downloads",
        f"{SCRIPTS}/network-install.test.sh", [
    (f"{SCRIPTS}/network-install.sh", "the script under test, which network-install.test.sh:39 runs"),
    (f"{SCRIPTS}/fetch.test.sh", f"{at('test', 'Test the bounds on network installs and downloads')} runs it "
                                 "after network-install.test.sh"),
    (f"{SCRIPTS}/fetch.sh", "the script fetch.test.sh tests, which fetch.test.sh:45 runs"),
])
