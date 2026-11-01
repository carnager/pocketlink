import QtQuick
import Quickshell
import Quickshell.Io
import qs.Common
import qs.Modals.FileBrowser
import qs.Modules.Plugins
import qs.Services
import qs.Widgets

PluginComponent {
    id: root

    pluginId: "pocketlink"
    popoutWidth: 400

    property bool pluginPopoutVisible: false
    property var status: null
    property var config: null
    property var overridden: []
    property string statusError: ""
    property string configError: ""
    property bool statusAttempted: false
    property bool statusStarted: false

    // Pairing: the QR image, when it expires, and which devices existed before.
    property string qrPath: ""
    property real qrExpiresAt: 0
    property int qrStamp: 0
    property int qrSecondsLeft: 0
    property var devicesBeforePairing: ({})

    property string confirmUnpairId: ""
    property var sendTarget: null // device to send the picked file to; null for all
    property string actionDescription: ""
    property string actionSuccess: "" // shown when the action worked, if set
    property string linkDraft: ""
    property string lastSentLink: "" // so the clipboard doesn't offer it again
    property bool settingsOpen: false

    readonly property string configuredBinary: String(loadPluginValue("pocketlinkBinary", "pocketlink")).trim()
    readonly property string pocketlinkBinary: configuredBinary.length > 0 ? configuredBinary : "pocketlink"
    readonly property string fileManager: String(loadPluginValue("fileManager", "")).trim()
    // The plugin's setting, else the daemon's (which "Show in folder" uses).
    readonly property string effectiveFileManager: fileManager.length > 0 ? fileManager : (config && config.file_manager ? config.file_manager : "")
    readonly property string filesUrl: status && status.files ? status.files : ""
    readonly property var devices: status && status.devices ? status.devices : []
    readonly property int connectedCount: devices.filter(d => d.connected).length
    readonly property bool pairing: qrPath.length > 0
    readonly property string qrFile: (Quickshell.env("XDG_RUNTIME_DIR") || "/tmp") + "/pocketlink-pair.png"
    // The emptiest connected phone's battery, for the bar; null if none says.
    readonly property var pillBattery: {
        let low = null;
        for (const d of devices) {
            if (d.connected && d.battery && (low === null || d.battery.level < low.level))
                low = d.battery;
        }
        return low;
    }
    readonly property bool pillBatteryLow: batteryLow(pillBattery)
    readonly property string pillIconName: connectedCount > 0 ? "phonelink" : "phonelink_off"
    readonly property string summaryText: {
        if (statusError.length > 0)
            return "Daemon not reachable";
        if (!status)
            return "Loading…";
        if (devices.length === 0)
            return "On " + status.name + " · no phone paired yet";
        if (devices.length === 1)
            return "On " + status.name;
        return "On " + status.name + " · " + connectedCount + " of " + devices.length + " connected";
    }

    function loadPluginValue(key, fallback) {
        const data = root.pluginData || ({});
        if (data[key] !== undefined)
            return data[key];

        if (root.pluginService && root.pluginService.loadPluginData)
            return root.pluginService.loadPluginData(root.pluginId, key, fallback);

        if (typeof PluginService !== "undefined" && PluginService.loadPluginData)
            return PluginService.loadPluginData(root.pluginId, key, fallback);

        return fallback;
    }

    // Go prints nanosecond timestamps, which JavaScript's Date can't parse.
    function parseTime(value) {
        const t = Date.parse(String(value || "").replace(/\.\d+/, ""));
        return isNaN(t) ? 0 : t;
    }

    function batteryIcon(b) {
        if (b.charging)
            return "battery_charging_full";
        if (b.level >= 95)
            return "battery_full";
        return "battery_" + Math.round(b.level / 100 * 6) + "_bar";
    }

    function batteryLow(b) {
        return !!b && b.level <= 15 && !b.charging;
    }

    function findPhone(device) {
        runAction(["find", "-to", device.id], "ring " + device.name, "Ringing " + device.name + " · stop it on the phone");
    }

    function lastSeenText(device) {
        const t = parseTime(device.last_seen);
        if (t === 0)
            return "never connected";
        const minutes = Math.round((Date.now() - t) / 60000);
        if (minutes < 1)
            return "seen just now";
        if (minutes < 60)
            return "seen " + minutes + " min ago";
        if (minutes < 60 * 24)
            return "seen " + Math.round(minutes / 60) + " h ago";
        return "seen " + new Date(t).toLocaleDateString();
    }

    function isOverridden(key) {
        return overridden.indexOf(key) >= 0;
    }

    function refreshStatus() {
        if (statusRunner.running)
            return;
        // A command that can't be found never starts and never exits, so
        // notice it on the next poll instead of showing "Loading…" forever.
        if (statusAttempted && !statusStarted)
            statusError = "Cannot run \"" + pocketlinkBinary + "\". Set the binary path in the plugin settings.";
        statusAttempted = true;
        statusStarted = false;
        statusRunner.command = [pocketlinkBinary, "status", "-json"];
        statusRunner.running = true;
    }

    function refreshConfig() {
        if (!configRunner.running) {
            configRunner.command = [pocketlinkBinary, "config", "-json"];
            configRunner.running = true;
        }
    }

    function runAction(args, description, success) {
        if (actionRunner.running) {
            ToastService.showWarning("pocketlink is busy, try again");
            return false;
        }
        actionDescription = description;
        actionSuccess = success || "";
        actionRunner.command = [pocketlinkBinary].concat(args);
        actionRunner.running = true;
        return true;
    }

    function startPairing() {
        if (pairRunner.running)
            return;
        const known = ({});
        for (const d of devices)
            known[d.id] = true;
        devicesBeforePairing = known;
        pairRunner.command = [pocketlinkBinary, "pair", "-json", "-png", qrFile];
        pairRunner.running = true;
    }

    function stopPairing() {
        qrPath = "";
        qrExpiresAt = 0;
    }

    function setConfig(key, value) {
        runAction(["config", key, String(value)], "change " + key);
    }

    function unpair(device) {
        if (confirmUnpairId !== device.id) {
            confirmUnpairId = device.id;
            confirmReset.restart();
            return;
        }
        confirmUnpairId = "";
        runAction(["unpair", device.id], "unpair " + device.name);
    }

    // Opens a link in the phones' browsers. A bare address such as
    // example.org gets https://; phones that are offline open it once they
    // connect.
    function sendLink() {
        let url = linkDraft.trim();
        if (url.length === 0)
            return;
        if (!/^[a-z][a-z0-9+.-]*:/i.test(url))
            url = "https://" + url;
        if (!/^https?:\/\/\S+$/i.test(url)) {
            ToastService.showError("Only http and https links can be sent");
            return;
        }
        const offline = devices.some(d => !d.connected);
        if (runAction(["link", url], "send the link", offline ? "The link opens on phones that are offline once they connect" : "Link sent")) {
            lastSentLink = linkDraft.trim();
            linkDraft = "";
        }
    }

    // Offers a link from the clipboard when the field is empty.
    function pasteLink() {
        if (linkDraft.length === 0 && devices.length > 0 && !clipboardRunner.running)
            clipboardRunner.running = true;
    }

    function sendFileTo(device) {
        sendTarget = device;
        openFileBrowser(sendBrowserLoader);
    }

    // Opens a WebDAV URL from pocketlink status in a file manager. KDE's
    // file managers call the scheme webdav://, GVFS-based ones dav://.
    function openFiles(url) {
        if (url.length === 0)
            return;
        filesRunner.command = ["sh", "-c", `
            fm=$1
            if [ -z "$fm" ]; then
                for c in dolphin thunar nautilus nemo pcmanfm-qt pcmanfm; do
                    command -v "$c" >/dev/null && { fm=$c; break; }
                done
            fi
            [ -n "$fm" ] || { echo "No file manager found. Set one in the plugin settings." >&2; exit 1; }
            case "$fm" in
                *dolphin*|*konqueror*|*krusader*) url="webdav://\${2#http://}" ;;
                *) url="dav://\${2#http://}" ;;
            esac
            command -v "\${fm%% *}" >/dev/null || { echo "$fm is not installed" >&2; exit 1; }
            setsid -f $fm "$url" >/dev/null 2>&1 </dev/null
        `, "sh", effectiveFileManager, url];
        filesRunner.running = true;
    }

    // The daemon opens received files' folders, so it gets the same file
    // manager as this plugin.
    function syncFileManager() {
        if (!config || fileManager.length === 0 || isOverridden("file_manager"))
            return;
        if ((config.file_manager || "") !== fileManager)
            runAction(["config", "file_manager", fileManager], "set the file manager");
    }

    onFileManagerChanged: syncFileManager()

    function openFileBrowser(loader) {
        loader.active = true;
        if (loader.item)
            loader.item.open();
    }

    Component.onCompleted: {
        refreshStatus();
        refreshConfig();
    }

    Timer {
        interval: root.pluginPopoutVisible || root.pairing ? 2000 : 15000
        repeat: true
        running: true
        onTriggered: root.refreshStatus()
    }

    Timer {
        id: confirmReset
        interval: 3000
        onTriggered: root.confirmUnpairId = ""
    }

    Timer {
        interval: 1000
        repeat: true
        running: root.pairing
        onTriggered: {
            root.qrSecondsLeft = Math.max(0, Math.round((root.qrExpiresAt - Date.now()) / 1000));
            if (root.qrSecondsLeft === 0)
                root.stopPairing();
        }
    }

    Process {
        id: statusRunner
        running: false
        onStarted: root.statusStarted = true

        stdout: StdioCollector {
            onStreamFinished: {
                if (text.trim().length === 0)
                    return;
                try {
                    root.status = JSON.parse(text);
                    root.statusError = "";
                } catch (e) {
                    root.statusError = "Unexpected output from pocketlink status";
                    return;
                }
                if (!root.pairing)
                    return;
                for (const d of root.devices) {
                    if (!root.devicesBeforePairing[d.id]) {
                        root.stopPairing();
                        ToastService.showInfo("Paired with " + d.name);
                        break;
                    }
                }
            }
        }

        stderr: StdioCollector {
            id: statusStderr
        }

        onExited: exitCode => {
            if (exitCode !== 0)
                root.statusError = statusStderr.text.trim() || ("Could not run " + root.pocketlinkBinary);
        }
    }

    Process {
        id: configRunner
        running: false

        stdout: StdioCollector {
            onStreamFinished: {
                if (text.trim().length === 0)
                    return;
                try {
                    const reply = JSON.parse(text);
                    root.config = reply.config;
                    root.overridden = reply.overridden || [];
                    root.configError = "";
                    root.syncFileManager();
                } catch (e) {
                    root.configError = "Unexpected output from pocketlink config";
                }
            }
        }

        stderr: StdioCollector {
            id: configStderr
        }

        onExited: exitCode => {
            if (exitCode === 0)
                return;
            const err = configStderr.text.trim();
            root.configError = err.indexOf("unknown command") >= 0
                ? "The running daemon is older than this plugin. Restart it to change settings here."
                : (err || "Could not read settings");
        }
    }

    Process {
        id: pairRunner
        running: false

        stdout: StdioCollector {
            onStreamFinished: {
                try {
                    const reply = JSON.parse(text);
                    root.qrExpiresAt = root.parseTime(reply.expires) || (Date.now() + 5 * 60000);
                    root.qrSecondsLeft = Math.round((root.qrExpiresAt - Date.now()) / 1000);
                    root.qrStamp = Date.now();
                    root.qrPath = reply.png;
                } catch (e) {
                }
            }
        }

        stderr: StdioCollector {
            id: pairStderr
        }

        onExited: exitCode => {
            if (exitCode !== 0)
                ToastService.showError("Could not start pairing", pairStderr.text.trim());
        }
    }

    Process {
        id: actionRunner
        running: false

        stdout: StdioCollector {
            id: actionStdout
        }

        stderr: StdioCollector {
            id: actionStderr
        }

        onExited: exitCode => {
            if (exitCode !== 0) {
                ToastService.showError("Could not " + root.actionDescription, actionStderr.text.trim());
            } else if (actionStdout.text.indexOf("Restart the daemon") >= 0) {
                ToastService.showInfo("Restart the pocketlink daemon to apply this change");
            } else if (root.actionSuccess.length > 0) {
                ToastService.showInfo(root.actionSuccess);
            }
            root.refreshStatus();
            root.refreshConfig();
        }
    }

    Process {
        id: clipboardRunner
        running: false
        // Text only: an image in the clipboard would come out as bytes.
        command: ["wl-paste", "--no-newline", "--type", "text"]

        stdout: StdioCollector {
            onStreamFinished: {
                const t = text.trim();
                if (root.linkDraft.length === 0 && t !== root.lastSentLink && t.length < 4096 && /^https?:\/\/\S+$/i.test(t))
                    root.linkDraft = t;
            }
        }
    }

    Process {
        id: filesRunner
        running: false

        stderr: StdioCollector {
            id: filesStderr
        }

        onExited: exitCode => {
            if (exitCode !== 0)
                ToastService.showError("Could not open the phone's files", filesStderr.text.trim());
        }
    }

    LazyLoader {
        id: sendBrowserLoader
        active: false

        FileBrowserModal {
            browserTitle: "Send file to phone"
            browserIcon: "upload_file"
            browserType: "generic"
            showHiddenFiles: false
            onFileSelected: path => {
                const name = path.split("/").pop();
                if (root.sendTarget)
                    root.runAction(["send", "-to", root.sendTarget.id, path], "send " + name + " to " + root.sendTarget.name);
                else
                    root.runAction(["send", path], "send " + name);
                close();
            }
        }
    }

    LazyLoader {
        id: folderBrowserLoader
        active: false

        FileBrowserModal {
            browserTitle: "Save received files to"
            browserIcon: "folder"
            browserType: "generic"
            folderMode: true
            showHiddenFiles: false
            onFileSelected: path => {
                root.setConfig("downloads", path);
                close();
            }
        }
    }

    component SettingLabel: Column {
        property string title
        property string subtitle
        property bool elideSubtitle: false

        spacing: 2

        StyledText {
            width: parent.width
            text: parent.title
            font.pixelSize: Theme.fontSizeMedium
            color: Theme.surfaceText
        }

        StyledText {
            visible: parent.subtitle.length > 0
            width: parent.width
            text: parent.subtitle
            font.pixelSize: Theme.fontSizeSmall
            color: Theme.surfaceVariantText
            elide: parent.elideSubtitle ? Text.ElideMiddle : Text.ElideNone
            wrapMode: parent.elideSubtitle ? Text.NoWrap : Text.WordWrap
        }
    }

    horizontalBarPill: Component {
        Rectangle {
            implicitWidth: Math.max(24, pillRow.implicitWidth + (root.pillBattery ? 12 : 0))
            implicitHeight: 24
            radius: 12
            color: root.pluginPopoutVisible ? Theme.widgetBaseHoverColor : "transparent"

            Row {
                id: pillRow
                anchors.centerIn: parent
                spacing: 3

                DankIcon {
                    anchors.verticalCenter: parent.verticalCenter
                    name: root.pillIconName
                    size: 15
                    color: root.connectedCount > 0 ? Theme.primary : Theme.widgetTextColor
                }

                StyledText {
                    visible: root.pillBattery !== null
                    anchors.verticalCenter: parent.verticalCenter
                    text: root.pillBattery ? root.pillBattery.level + "%" : ""
                    font.pixelSize: Theme.fontSizeSmall
                    color: root.pillBatteryLow ? Theme.error : Theme.widgetTextColor
                }

                DankIcon {
                    visible: root.pillBattery !== null && root.pillBattery.charging
                    anchors.verticalCenter: parent.verticalCenter
                    name: "bolt"
                    size: 12
                    color: Theme.widgetTextColor
                }
            }

            Rectangle {
                visible: root.statusError.length > 0
                width: 7
                height: 7
                radius: 3.5
                anchors.right: parent.right
                anchors.bottom: parent.bottom
                anchors.rightMargin: 2
                anchors.bottomMargin: 2
                color: Theme.error
            }
        }
    }

    verticalBarPill: Component {
        Rectangle {
            width: Theme.barIconSize(root.barThickness)
            height: Math.max(Theme.barIconSize(root.barThickness), pillColumn.implicitHeight + (root.pillBattery ? 6 : 0))
            radius: Theme.cornerRadius
            color: root.pluginPopoutVisible ? Theme.widgetBaseHoverColor : "transparent"

            Column {
                id: pillColumn
                anchors.centerIn: parent

                DankIcon {
                    anchors.horizontalCenter: parent.horizontalCenter
                    name: root.pillIconName
                    size: Math.max(14, Theme.barIconSize(root.barThickness) - 4)
                    color: root.connectedCount > 0 ? Theme.primary : Theme.widgetIconColor
                }

                // Just the number: a vertical bar has no room for "%".
                StyledText {
                    visible: root.pillBattery !== null
                    anchors.horizontalCenter: parent.horizontalCenter
                    text: root.pillBattery ? String(root.pillBattery.level) : ""
                    font.pixelSize: Theme.fontSizeSmall
                    color: root.pillBatteryLow ? Theme.error : (root.pillBattery && root.pillBattery.charging ? Theme.primary : Theme.widgetTextColor)
                }
            }
        }
    }

    popoutContent: Component {
        Item {
            id: popoutRoot
            property var parentPopout: null

            implicitWidth: root.popoutWidth
            implicitHeight: Math.min(contentColumn.implicitHeight + Theme.spacingL * 2, 820)

            Connections {
                target: popoutRoot.parentPopout
                function onShouldBeVisibleChanged() {
                    root.pluginPopoutVisible = !!(popoutRoot.parentPopout && popoutRoot.parentPopout.shouldBeVisible);
                    if (root.pluginPopoutVisible) {
                        root.refreshStatus();
                        root.refreshConfig();
                        root.pasteLink();
                    } else {
                        root.confirmUnpairId = "";
                    }
                }
            }

            StyledRect {
                anchors.fill: parent
                radius: Theme.cornerRadius
                color: Theme.surfaceContainer
                border.color: Theme.outline
                border.width: 1
            }

            Flickable {
                anchors.fill: parent
                anchors.margins: Theme.spacingL
                contentWidth: width
                contentHeight: contentColumn.implicitHeight
                clip: true
                boundsBehavior: Flickable.StopAtBounds

                Column {
                    id: contentColumn

                    width: parent.width
                    spacing: Theme.spacingM

                    // Header: what this computer is called, and pairing.
                    Item {
                        width: parent.width
                        height: Math.max(headerText.implicitHeight, pairButton.height)

                        Column {
                            id: headerText
                            anchors.left: parent.left
                            anchors.right: pairButton.left
                            anchors.rightMargin: Theme.spacingS
                            anchors.verticalCenter: parent.verticalCenter
                            spacing: 2

                            StyledText {
                                width: parent.width
                                text: "pocketlink"
                                font.pixelSize: Theme.fontSizeLarge
                                font.weight: Font.Medium
                                color: Theme.surfaceText
                            }

                            StyledText {
                                width: parent.width
                                text: root.summaryText
                                font.pixelSize: Theme.fontSizeSmall
                                color: Theme.surfaceVariantText
                                elide: Text.ElideRight
                            }
                        }

                        DankActionButton {
                            id: pairButton
                            anchors.right: parent.right
                            anchors.verticalCenter: parent.verticalCenter
                            iconName: root.pairing ? "close" : "qr_code_2"
                            iconColor: Theme.surfaceText
                            tooltipText: root.pairing ? "Cancel pairing" : "Pair a phone"
                            enabled: root.statusError.length === 0
                            onClicked: root.pairing ? root.stopPairing() : root.startPairing()
                        }
                    }

                    StyledText {
                        visible: root.statusError.length > 0
                        width: parent.width
                        text: root.statusStarted ? root.statusError + "\nStart it with: systemctl --user start pocketlink" : root.statusError
                        font.pixelSize: Theme.fontSizeSmall
                        color: Theme.error
                        wrapMode: Text.WordWrap
                    }

                    Column {
                        visible: root.pairing
                        width: parent.width
                        spacing: Theme.spacingS

                        // QR codes need a light background to scan, whatever the theme.
                        Rectangle {
                            anchors.horizontalCenter: parent.horizontalCenter
                            width: 248
                            height: 248
                            radius: Theme.cornerRadius
                            color: "white"

                            Image {
                                anchors.fill: parent
                                anchors.margins: 4
                                source: root.pairing ? "file://" + root.qrPath + "?" + root.qrStamp : ""
                                cache: false
                                smooth: false
                                fillMode: Image.PreserveAspectFit
                            }
                        }

                        StyledText {
                            width: parent.width
                            horizontalAlignment: Text.AlignHCenter
                            text: "Scan with the pocketlink app · expires in "
                                + Math.floor(root.qrSecondsLeft / 60) + ":" + String(root.qrSecondsLeft % 60).padStart(2, "0")
                            font.pixelSize: Theme.fontSizeSmall
                            color: Theme.surfaceVariantText
                        }
                    }

                    Column {
                        visible: root.devices.length > 0
                        width: parent.width
                        spacing: Theme.spacingS

                        Repeater {
                            model: root.devices

                            delegate: Rectangle {
                                id: card
                                required property var modelData
                                readonly property var battery: modelData.connected ? modelData.battery : null
                                readonly property bool confirming: root.confirmUnpairId === modelData.id

                                width: parent.width
                                height: 60
                                radius: Theme.cornerRadius
                                color: Theme.surfaceContainerHigh

                                Rectangle {
                                    id: deviceIcon
                                    anchors.left: parent.left
                                    anchors.leftMargin: Theme.spacingM
                                    anchors.verticalCenter: parent.verticalCenter
                                    width: 36
                                    height: 36
                                    radius: 18
                                    color: card.modelData.connected ? Theme.primaryContainer : Theme.surfaceContainerHighest

                                    DankIcon {
                                        anchors.centerIn: parent
                                        name: "smartphone"
                                        size: 20
                                        color: card.modelData.connected ? Theme.primary : Theme.surfaceVariantText
                                    }
                                }

                                Column {
                                    anchors.left: deviceIcon.right
                                    anchors.leftMargin: Theme.spacingM
                                    anchors.right: rowButtons.left
                                    anchors.rightMargin: Theme.spacingS
                                    anchors.verticalCenter: parent.verticalCenter
                                    spacing: 2

                                    StyledText {
                                        width: parent.width
                                        text: card.modelData.name
                                        font.pixelSize: Theme.fontSizeMedium
                                        font.weight: Font.Medium
                                        color: Theme.surfaceText
                                        elide: Text.ElideRight
                                    }

                                    Row {
                                        width: parent.width
                                        spacing: Theme.spacingXS

                                        DankIcon {
                                            visible: card.battery !== null
                                            anchors.verticalCenter: parent.verticalCenter
                                            name: card.battery ? root.batteryIcon(card.battery) : ""
                                            size: 14
                                            color: root.batteryLow(card.battery) ? Theme.error : Theme.surfaceVariantText
                                        }

                                        StyledText {
                                            visible: card.battery !== null
                                            anchors.verticalCenter: parent.verticalCenter
                                            text: card.battery ? card.battery.level + "%" : ""
                                            font.pixelSize: Theme.fontSizeSmall
                                            color: root.batteryLow(card.battery) ? Theme.error : Theme.surfaceVariantText
                                        }

                                        StyledText {
                                            visible: !card.modelData.connected || card.modelData.pending > 0
                                            anchors.verticalCenter: parent.verticalCenter
                                            text: (card.battery !== null ? "· " : "")
                                                + (card.modelData.connected ? "" : root.lastSeenText(card.modelData))
                                                + (!card.modelData.connected && card.modelData.pending > 0 ? " · " : "")
                                                + (card.modelData.pending > 0 ? card.modelData.pending + " queued" : "")
                                            font.pixelSize: Theme.fontSizeSmall
                                            color: Theme.surfaceVariantText
                                            elide: Text.ElideRight
                                        }
                                    }
                                }

                                // With one phone, sending files and opening its files are
                                // the buttons below; per phone they only matter with several.
                                Row {
                                    id: rowButtons
                                    anchors.right: parent.right
                                    anchors.rightMargin: Theme.spacingS
                                    anchors.verticalCenter: parent.verticalCenter
                                    spacing: 2

                                    DankActionButton {
                                        // Only a connected phone can ring.
                                        visible: card.modelData.connected
                                        iconName: "phonelink_ring"
                                        iconColor: Theme.surfaceText
                                        tooltipText: "Ring the phone"
                                        onClicked: root.findPhone(card.modelData)
                                    }

                                    DankActionButton {
                                        visible: !!card.modelData.files && root.devices.length > 1
                                        enabled: card.modelData.connected
                                        iconName: "folder_open"
                                        iconColor: Theme.surfaceText
                                        tooltipText: "Phone files"
                                        onClicked: root.openFiles(card.modelData.files)
                                    }

                                    DankActionButton {
                                        visible: root.devices.length > 1
                                        iconName: "upload_file"
                                        iconColor: Theme.surfaceText
                                        tooltipText: "Send a file"
                                        onClicked: root.sendFileTo(card.modelData)
                                    }

                                    DankActionButton {
                                        iconName: card.confirming ? "check" : "link_off"
                                        iconColor: card.confirming ? Theme.error : Theme.surfaceVariantText
                                        backgroundColor: card.confirming ? Theme.withAlpha(Theme.error, 0.15) : "transparent"
                                        tooltipText: card.confirming ? "Click again to unpair" : "Unpair"
                                        onClicked: root.unpair(card.modelData)
                                    }
                                }
                            }
                        }
                    }

                    Row {
                        id: actionRow
                        readonly property int buttons: root.filesUrl.length > 0 ? 2 : 1

                        visible: root.devices.length > 0
                        width: parent.width
                        spacing: Theme.spacingS

                        DankButton {
                            width: (parent.width - Theme.spacingS * (actionRow.buttons - 1)) / actionRow.buttons
                            text: root.devices.length > 1 ? "Send to all" : "Send file"
                            iconName: "upload_file"
                            onClicked: root.sendFileTo(null)
                        }

                        DankButton {
                            visible: root.filesUrl.length > 0
                            width: (parent.width - Theme.spacingS * (actionRow.buttons - 1)) / actionRow.buttons
                            text: "Phone files"
                            iconName: "folder_open"
                            backgroundColor: Theme.surfaceContainerHigh
                            textColor: Theme.surfaceText
                            // One phone: straight into its folder rather than the list of phones.
                            onClicked: root.openFiles(root.devices.length === 1 && root.devices[0].files ? root.devices[0].files : root.filesUrl)
                        }
                    }

                    // A link to open in the phones' browsers.
                    DankTextField {
                        id: linkField
                        visible: root.devices.length > 0
                        width: parent.width
                        leftIconName: "link"
                        placeholderText: root.devices.length > 1 ? "Link to open on all phones" : "Link to open on the phone"
                        text: root.linkDraft
                        onTextEdited: root.linkDraft = text
                        onAccepted: root.sendLink()
                        trailingContent: Component {
                            DankActionButton {
                                iconName: "open_in_browser"
                                iconColor: root.linkDraft.trim().length > 0 ? Theme.primary : Theme.surfaceVariantText
                                enabled: root.linkDraft.trim().length > 0
                                tooltipText: "Open on the phone"
                                onClicked: root.sendLink()
                            }
                        }
                    }

                    // Settings, folded away: they rarely change.
                    Rectangle {
                        width: parent.width
                        height: 40
                        radius: Theme.cornerRadius
                        color: settingsHeaderArea.containsMouse ? Theme.surfaceContainerHigh : "transparent"

                        DankIcon {
                            id: settingsIcon
                            anchors.left: parent.left
                            anchors.leftMargin: Theme.spacingS
                            anchors.verticalCenter: parent.verticalCenter
                            name: "settings"
                            size: 18
                            color: Theme.surfaceVariantText
                        }

                        StyledText {
                            anchors.left: settingsIcon.right
                            anchors.leftMargin: Theme.spacingS
                            anchors.verticalCenter: parent.verticalCenter
                            text: "Settings"
                            font.pixelSize: Theme.fontSizeMedium
                            color: Theme.surfaceText
                        }

                        DankIcon {
                            anchors.right: parent.right
                            anchors.rightMargin: Theme.spacingS
                            anchors.verticalCenter: parent.verticalCenter
                            name: "expand_more"
                            size: 20
                            color: Theme.surfaceVariantText
                            rotation: root.settingsOpen ? 180 : 0

                            Behavior on rotation {
                                NumberAnimation {
                                    duration: 150
                                }
                            }
                        }

                        MouseArea {
                            id: settingsHeaderArea
                            anchors.fill: parent
                            hoverEnabled: true
                            cursorShape: Qt.PointingHandCursor
                            onClicked: root.settingsOpen = !root.settingsOpen
                        }
                    }

                    Column {
                        visible: root.settingsOpen
                        width: parent.width
                        spacing: Theme.spacingM

                        StyledText {
                            visible: root.configError.length > 0 && root.statusError.length === 0
                            width: parent.width
                            text: root.configError
                            font.pixelSize: Theme.fontSizeSmall
                            color: Theme.error
                            wrapMode: Text.WordWrap
                        }

                        Item {
                            width: parent.width
                            height: Math.max(folderLabel.implicitHeight, changeButton.height)

                            SettingLabel {
                                id: folderLabel
                                anchors.left: parent.left
                                anchors.right: changeButton.left
                                anchors.rightMargin: Theme.spacingM
                                anchors.verticalCenter: parent.verticalCenter
                                title: "Received files"
                                subtitle: root.config ? root.config.downloads : "unavailable"
                                elideSubtitle: true
                            }

                            DankActionButton {
                                id: changeButton
                                anchors.right: parent.right
                                anchors.verticalCenter: parent.verticalCenter
                                iconName: "drive_folder_upload"
                                iconColor: Theme.surfaceText
                                tooltipText: "Change folder"
                                enabled: root.config !== null && !root.isOverridden("downloads")
                                onClicked: root.openFileBrowser(folderBrowserLoader)
                            }
                        }

                        Item {
                            width: parent.width
                            height: Math.max(clipLabel.implicitHeight, clipToggle.height)

                            SettingLabel {
                                id: clipLabel
                                anchors.left: parent.left
                                anchors.right: clipToggle.left
                                anchors.rightMargin: Theme.spacingM
                                anchors.verticalCenter: parent.verticalCenter
                                title: "Sync desktop clipboard"
                                subtitle: "Copying on the desktop updates the phone"
                            }

                            DankToggle {
                                id: clipToggle
                                anchors.right: parent.right
                                anchors.verticalCenter: parent.verticalCenter
                                hideText: true
                                checked: root.config ? root.config.clipboard : false
                                enabled: root.config !== null && !root.isOverridden("clipboard")
                                onToggled: checked => root.setConfig("clipboard", checked)
                            }
                        }

                        Repeater {
                            model: [
                                { "key": "ring_action", "label": "While the phone rings" },
                                { "key": "talk_action", "label": "During a call" }
                            ]

                            delegate: Column {
                                id: callSetting
                                required property var modelData
                                readonly property var values: ["pause", "lower", "none"]

                                width: parent.width
                                spacing: Theme.spacingXS

                                SettingLabel {
                                    width: parent.width
                                    title: callSetting.modelData.label
                                }

                                DankButtonGroup {
                                    width: parent.width
                                    fillWidth: true
                                    size: "small"
                                    checkEnabled: false
                                    arrowKeysSelect: false
                                    enabled: root.config !== null
                                    model: ["Pause media", "Lower volume", "Nothing"]
                                    currentIndex: root.config ? callSetting.values.indexOf(root.config[callSetting.modelData.key]) : -1
                                    onSelectionChanged: (index, selected) => {
                                        if (selected && root.config && root.config[callSetting.modelData.key] !== callSetting.values[index])
                                            root.setConfig(callSetting.modelData.key, callSetting.values[index]);
                                    }
                                }
                            }
                        }

                        Column {
                            visible: root.config !== null && (root.config.ring_action === "lower" || root.config.talk_action === "lower")
                            width: parent.width
                            spacing: Theme.spacingXS

                            Item {
                                width: parent.width
                                height: volumeLabel.implicitHeight

                                SettingLabel {
                                    id: volumeLabel
                                    anchors.left: parent.left
                                    anchors.right: volumeValue.left
                                    title: "Lowered volume"
                                    subtitle: "Share of the volume before the call"
                                }

                                StyledText {
                                    id: volumeValue
                                    anchors.right: parent.right
                                    anchors.verticalCenter: parent.verticalCenter
                                    text: volumeSlider.value + "%"
                                    font.pixelSize: Theme.fontSizeMedium
                                    color: Theme.surfaceText
                                }
                            }

                            DankSlider {
                                id: volumeSlider
                                width: parent.width
                                minimum: 5
                                maximum: 90
                                step: 5
                                showValue: false
                                value: root.config ? root.config.call_volume : 40
                                onSliderValueChanged: newValue => value = newValue
                                onSliderDragFinished: finalValue => root.setConfig("call_volume", finalValue)
                            }
                        }

                        StyledText {
                            visible: root.overridden.length > 0
                            width: parent.width
                            text: "Set by daemon flags: " + root.overridden.join(", ")
                            font.pixelSize: Theme.fontSizeSmall
                            color: Theme.surfaceVariantText
                            wrapMode: Text.WordWrap
                        }
                    }
                }
            }
        }
    }
}
