# Upgrade and uninstall

## Upgrade

Run the same installer again; config, secrets and logs are kept, and the
service is restarted with the new binary.

=== "Without root"

    ```bash
    curl -fsSL https://nimdeploy.nimbox360.com/install.sh | sh
    ```

=== "Service account"

    ```bash
    sudo bash setup-root.sh --service-user deploy --admin-user aitor   # same options as the first time
    ```

=== "As root"

    ```bash
    curl -fsSL https://nimdeploy.nimbox360.com/install.sh | sudo sh
    ```

Check the running version with `nimdeploy -version`. Release notes:
[GitHub releases](https://github.com/aitorroma/nimdeploy/releases).

!!! tip
    A restart stops running deploys (`shutdown_timeout` gives them up to 5
    minutes to finish first; a push queued at that moment is lost and logged).
    Upgrade when nothing is deploying: `nimdeploy status`.

## Uninstall

=== "Without root"

    ```bash
    nimdeploy uninstall            # keeps config, secrets and logs
    nimdeploy uninstall --purge    # removes them too
    ```

=== "Service account"

    ```bash
    sudo bash setup-root.sh --uninstall
    ```

    Removes both units and the sudoers file. The account's files
    (`~deploy/.local/bin`, `~deploy/.config/nimdeploy`, `~deploy/.pm2`) and
    `/var/log/nimdeploy` are kept; the operator stays in `systemd-journal`
    (`gpasswd -d aitor systemd-journal`).

=== "As root"

    ```bash
    sudo ./install.sh uninstall            # keeps /etc/nimdeploy and the logs
    sudo ./install.sh uninstall --purge    # removes them too
    ```

Remember to delete the webhooks in your git host as well.
