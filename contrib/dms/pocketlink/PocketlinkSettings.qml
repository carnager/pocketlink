import QtQuick
import qs.Common
import qs.Modules.Plugins
import qs.Widgets

PluginSettings {
    id: root

    pluginId: "pocketlink"

    StyledText {
        width: parent.width
        text: "pocketlink"
        font.pixelSize: Theme.fontSizeLarge
        font.weight: Font.Bold
        color: Theme.surfaceText
    }

    StyledText {
        width: parent.width
        text: "Pairing, the download folder and clipboard sync are managed from the widget popout. They are stored by the pocketlink daemon itself."
        font.pixelSize: Theme.fontSizeSmall
        color: Theme.surfaceVariantText
        wrapMode: Text.WordWrap
    }

    StyledRect {
        width: parent.width
        height: 1
        color: Theme.surfaceVariant
    }

    StringSetting {
        settingKey: "pocketlinkBinary"
        label: "pocketlink Binary"
        description: "Path or command name for the pocketlink CLI."
        placeholder: "pocketlink"
        defaultValue: "pocketlink"
    }
}
