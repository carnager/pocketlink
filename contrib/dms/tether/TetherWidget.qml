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

    pluginId: "tether"
    popoutWidth: 400
    popoutHeight: 680

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
    property string actionDescription: ""

    readonly property string configuredBinary: String(loadPluginValue("tetherBinary", "tether")).trim()
    readonly property string tetherBinary: configuredBinary.length > 0 ? configuredBinary : "tether"
    readonly property var devices: status && status.devices ? status.devices : []
    readonly property int connectedCount: devices.filter(d => d.connected).length
    readonly property bool pairing: qrPath.length > 0
    readonly property string qrFile: (Quickshell.env("XDG_RUNTIME_DIR") || "/tmp") + "/tether-pair.png"
    readonly property string pillIconName: connectedCount > 0 ? "phonelink" : "phonelink_off"
    readonly property string summaryText: {
        if (statusError.length > 0)
            return "Daemon not reachable";
        if (!status)
            return "Loading…";
        if (devices.length === 0)
            return status.name + " · no phone paired";
        return status.name + " · " + connectedCount + " of " + devices.length + " connected";
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
            statusError = "Cannot run \"" + tetherBinary + "\". Set the binary path in the plugin settings.";
        statusAttempted = true;
        statusStarted = false;
        statusRunner.command = [tetherBinary, "status", "-json"];
        statusRunner.running = true;
    }

    function refreshConfig() {
        if (!configRunner.running) {
            configRunner.command = [tetherBinary, "config", "-json"];
            configRunner.running = true;
        }
    }

    function runAction(args, description) {
        if (actionRunner.running) {
            ToastService.showWarning("tether is busy, try again");
            return;
        }
        actionDescription = description;
        actionRunner.command = [tetherBinary].concat(args);
        actionRunner.running = true;
    }

    function startPairing() {
        if (pairRunner.running)
            return;
        const known = ({});
        for (const d of devices)
            known[d.id] = true;
        devicesBeforePairing = known;
        pairRunner.command = [tetherBinary, "pair", "-json", "-png", qrFile];
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

    function openFileBrowser(loader) {
        loader.active = true;
        if (loader.item)
            loader.item.open();
    }

    Component.onCompleted: refreshStatus()

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
                    root.statusError = "Unexpected output from tether status";
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
                root.statusError = statusStderr.text.trim() || ("Could not run " + root.tetherBinary);
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
                } catch (e) {
                    root.configError = "Unexpected output from tether config";
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
                ToastService.showInfo("Restart the tether daemon to apply this change");
            }
            root.refreshStatus();
            root.refreshConfig();
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
                root.runAction(["send", path], "send " + path.split("/").pop());
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

    horizontalBarPill: Component {
        Rectangle {
            width: 24
            height: 24
            radius: 12
            color: root.pluginPopoutVisible ? Theme.widgetBaseHoverColor : "transparent"

            DankIcon {
                anchors.centerIn: parent
                name: root.pillIconName
                size: 15
                color: root.connectedCount > 0 ? Theme.primary : Theme.widgetTextColor
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
            height: Theme.barIconSize(root.barThickness)
            radius: Theme.cornerRadius
            color: root.pluginPopoutVisible ? Theme.widgetBaseHoverColor : "transparent"

            DankIcon {
                anchors.centerIn: parent
                name: root.pillIconName
                size: Math.max(14, Theme.barIconSize(root.barThickness) - 4)
                color: root.connectedCount > 0 ? Theme.primary : Theme.widgetIconColor
            }
        }
    }

    popoutContent: Component {
        Item {
            id: popoutRoot
            property var parentPopout: null

            implicitWidth: root.popoutWidth
            implicitHeight: root.popoutHeight

            Connections {
                target: popoutRoot.parentPopout
                function onShouldBeVisibleChanged() {
                    root.pluginPopoutVisible = !!(popoutRoot.parentPopout && popoutRoot.parentPopout.shouldBeVisible);
                    if (root.pluginPopoutVisible) {
                        root.refreshStatus();
                        root.refreshConfig();
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
                anchors.margins: Theme.spacingM
                contentWidth: width
                contentHeight: contentColumn.implicitHeight
                clip: true
                boundsBehavior: Flickable.StopAtBounds

                Column {
                    id: contentColumn

                    width: parent.width
                    spacing: Theme.spacingS

                    StyledText {
                        width: parent.width
                        text: "tether"
                        font.pixelSize: Theme.fontSizeLarge
                        font.weight: Font.Bold
                        color: Theme.surfaceText
                    }

                    StyledText {
                        width: parent.width
                        text: root.summaryText
                        font.pixelSize: Theme.fontSizeSmall
                        color: Theme.surfaceVariantText
                    }

                    StyledText {
                        visible: root.statusError.length > 0
                        width: parent.width
                        text: root.statusStarted ? root.statusError + "\nStart it with: systemctl --user start tether" : root.statusError
                        font.pixelSize: Theme.fontSizeSmall
                        color: Theme.error
                        wrapMode: Text.WordWrap
                    }

                    Repeater {
                        model: root.devices

                        delegate: Rectangle {
                            required property var modelData

                            width: contentColumn.width
                            height: 56
                            radius: 12
                            color: Theme.surfaceContainerHigh
                            border.color: Theme.outline
                            border.width: 1

                            DankIcon {
                                id: deviceIcon
                                anchors.left: parent.left
                                anchors.leftMargin: Theme.spacingM
                                anchors.verticalCenter: parent.verticalCenter
                                name: "smartphone"
                                size: 20
                                color: modelData.connected ? Theme.primary : Theme.surfaceVariantText
                            }

                            Column {
                                anchors.left: deviceIcon.right
                                anchors.leftMargin: Theme.spacingM
                                anchors.right: unpairButton.left
                                anchors.rightMargin: Theme.spacingS
                                anchors.verticalCenter: parent.verticalCenter
                                spacing: 2

                                StyledText {
                                    width: parent.width
                                    text: modelData.name
                                    font.pixelSize: Theme.fontSizeMedium
                                    color: Theme.surfaceText
                                    elide: Text.ElideRight
                                }

                                StyledText {
                                    width: parent.width
                                    text: (modelData.connected ? "Connected" : root.lastSeenText(modelData))
                                        + (modelData.pending > 0 ? " · " + modelData.pending + " queued" : "")
                                    font.pixelSize: Theme.fontSizeSmall
                                    color: Theme.surfaceVariantText
                                    elide: Text.ElideRight
                                }
                            }

                            Rectangle {
                                id: unpairButton

                                readonly property bool confirming: root.confirmUnpairId === modelData.id

                                anchors.right: parent.right
                                anchors.rightMargin: Theme.spacingM
                                anchors.verticalCenter: parent.verticalCenter
                                width: confirming ? 84 : 64
                                height: 30
                                radius: 10
                                color: unpairArea.containsMouse ? Theme.widgetBaseHoverColor : Theme.surfaceContainer
                                border.color: confirming ? Theme.error : Theme.outline
                                border.width: 1

                                StyledText {
                                    anchors.centerIn: parent
                                    text: unpairButton.confirming ? "Confirm?" : "Unpair"
                                    font.pixelSize: Theme.fontSizeSmall
                                    color: unpairButton.confirming ? Theme.error : Theme.surfaceText
                                }

                                MouseArea {
                                    id: unpairArea
                                    anchors.fill: parent
                                    hoverEnabled: true
                                    cursorShape: Qt.PointingHandCursor
                                    onClicked: root.unpair(modelData)
                                }
                            }
                        }
                    }

                    Row {
                        width: parent.width
                        spacing: Theme.spacingS

                        DankButton {
                            width: (parent.width - Theme.spacingS) / 2
                            text: root.pairing ? "Cancel pairing" : "Pair phone"
                            iconName: root.pairing ? "close" : "qr_code_2"
                            enabled: root.statusError.length === 0
                            onClicked: root.pairing ? root.stopPairing() : root.startPairing()
                        }

                        DankButton {
                            width: (parent.width - Theme.spacingS) / 2
                            text: "Send file"
                            iconName: "upload_file"
                            enabled: root.devices.length > 0
                            onClicked: root.openFileBrowser(sendBrowserLoader)
                        }
                    }

                    Column {
                        visible: root.pairing
                        width: parent.width
                        spacing: Theme.spacingS
                        topPadding: Theme.spacingS

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
                            text: "Scan with the tether app · expires in "
                                + Math.floor(root.qrSecondsLeft / 60) + ":" + String(root.qrSecondsLeft % 60).padStart(2, "0")
                            font.pixelSize: Theme.fontSizeSmall
                            color: Theme.surfaceVariantText
                        }
                    }

                    StyledRect {
                        width: parent.width
                        height: 1
                        color: Theme.surfaceVariant
                    }

                    StyledText {
                        width: parent.width
                        text: "Settings"
                        font.pixelSize: Theme.fontSizeMedium
                        font.weight: Font.Bold
                        color: Theme.surfaceText
                    }

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
                        height: Math.max(folderText.implicitHeight, changeFolderButton.height)

                        Column {
                            id: folderText
                            anchors.left: parent.left
                            anchors.right: changeFolderButton.left
                            anchors.rightMargin: Theme.spacingS
                            anchors.verticalCenter: parent.verticalCenter
                            spacing: 2

                            StyledText {
                                width: parent.width
                                text: "Received files"
                                font.pixelSize: Theme.fontSizeMedium
                                color: Theme.surfaceText
                            }

                            StyledText {
                                width: parent.width
                                text: root.config ? root.config.downloads : "unavailable"
                                font.pixelSize: Theme.fontSizeSmall
                                color: Theme.surfaceVariantText
                                elide: Text.ElideMiddle
                            }
                        }

                        DankButton {
                            id: changeFolderButton
                            anchors.right: parent.right
                            anchors.verticalCenter: parent.verticalCenter
                            text: "Change"
                            enabled: root.config !== null && !root.isOverridden("downloads")
                            onClicked: root.openFileBrowser(folderBrowserLoader)
                        }
                    }

                    DankToggle {
                        width: parent.width
                        text: "Sync desktop clipboard"
                        description: "Copying on the desktop updates the phone's clipboard"
                        checked: root.config ? root.config.clipboard : false
                        enabled: root.config !== null && !root.isOverridden("clipboard")
                        onToggled: checked => root.setConfig("clipboard", checked)
                    }

                    StyledText {
                        width: parent.width
                        text: "During phone calls"
                        font.pixelSize: Theme.fontSizeMedium
                        color: Theme.surfaceText
                    }

                    Row {
                        width: parent.width
                        spacing: Theme.spacingS

                        Repeater {
                            model: [
                                { "value": "pause", "label": "Pause media" },
                                { "value": "lower", "label": "Lower volume" },
                                { "value": "none", "label": "Nothing" }
                            ]

                            delegate: Rectangle {
                                required property var modelData
                                readonly property bool selected: root.config !== null && root.config.call_action === modelData.value

                                width: (parent.width - Theme.spacingS * 2) / 3
                                height: 32
                                radius: 10
                                color: selected ? Theme.primary : (callOptionArea.containsMouse ? Theme.widgetBaseHoverColor : Theme.surfaceContainer)
                                border.color: selected ? Theme.primary : Theme.outline
                                border.width: 1
                                opacity: root.config !== null ? 1 : 0.5

                                StyledText {
                                    anchors.centerIn: parent
                                    text: modelData.label
                                    font.pixelSize: Theme.fontSizeSmall
                                    color: parent.selected ? Theme.primaryText : Theme.surfaceText
                                }

                                MouseArea {
                                    id: callOptionArea
                                    anchors.fill: parent
                                    hoverEnabled: true
                                    enabled: root.config !== null && !parent.selected
                                    cursorShape: enabled ? Qt.PointingHandCursor : Qt.ArrowCursor
                                    onClicked: root.setConfig("call_action", modelData.value)
                                }
                            }
                        }
                    }

                    StyledText {
                        visible: root.config !== null && root.config.call_action === "lower"
                        width: parent.width
                        text: "Volume during calls: " + (root.config ? root.config.call_volume : 0) + "% (tether config call_volume N)"
                        font.pixelSize: Theme.fontSizeSmall
                        color: Theme.surfaceVariantText
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
