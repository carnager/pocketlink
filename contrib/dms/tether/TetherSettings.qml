import QtQuick
import qs.Common
import qs.Modules.Plugins
import qs.Widgets

PluginSettings {
    id: root

    pluginId: "tether"

    StyledText {
        width: parent.width
        text: "tether"
        font.pixelSize: Theme.fontSizeLarge
        font.weight: Font.Bold
        color: Theme.surfaceText
    }

    StyledText {
        width: parent.width
        text: "Pairing, the download folder and clipboard sync are managed from the widget popout. They are stored by the tether daemon itself."
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
        settingKey: "tetherBinary"
        label: "tether Binary"
        description: "Path or command name for the tether CLI."
        placeholder: "tether"
        defaultValue: "tether"
    }
}
