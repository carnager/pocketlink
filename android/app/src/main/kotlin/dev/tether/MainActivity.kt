package dev.tether

import android.Manifest
import android.app.NotificationManager
import android.app.StatusBarManager
import android.content.ComponentName
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Bitmap
import android.graphics.Canvas
import android.graphics.drawable.Icon
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.PowerManager
import android.provider.DocumentsContract
import android.provider.Settings
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.rounded.ArrowBack
import androidx.compose.material.icons.rounded.BatteryAlert
import androidx.compose.material.icons.rounded.ContentPaste
import androidx.compose.material.icons.rounded.ErrorOutline
import androidx.compose.material.icons.rounded.Folder
import androidx.compose.material.icons.rounded.Link
import androidx.compose.material.icons.rounded.LinkOff
import androidx.compose.material.icons.rounded.Notifications
import androidx.compose.material.icons.rounded.NotificationsActive
import androidx.compose.material.icons.rounded.QrCodeScanner
import androidx.compose.material.icons.rounded.Restore
import androidx.compose.material.icons.rounded.Sync
import androidx.compose.material.icons.rounded.Tune
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LargeTopAppBar
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.ListItem
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.input.nestedscroll.nestedScroll
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import com.journeyapps.barcodescanner.CaptureActivity
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanOptions

/** The scanner library's capture screen, locked to portrait in the manifest. */
class PortraitCaptureActivity : CaptureActivity()

private enum class Screen { MAIN, APPS }

private class Requirement(
    val icon: ImageVector,
    val title: String,
    val why: String,
    val grant: () -> Unit,
)

class MainActivity : ComponentActivity() {
    private val link by lazy { Link.get(this) }

    private var status by mutableStateOf(Status.UNPAIRED)
    private var pairingNote by mutableStateOf<String?>(null)
    private var screen by mutableStateOf(Screen.MAIN)
    // Bumped on resume and after changes, so permission and settings rows re-read their state.
    private var tick by mutableIntStateOf(0)
    private var unobserve: (() -> Unit)? = null

    private val scan = registerForActivityResult(ScanContract()) { r -> r.contents?.let(::pair) }
    private val askNotify = registerForActivityResult(ActivityResultContracts.RequestPermission()) { tick++ }
    private val pickFolder = registerForActivityResult(ActivityResultContracts.OpenDocumentTree()) { tree ->
        if (tree != null) {
            // Keep access across reboots.
            contentResolver.takePersistableUriPermission(
                tree, Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_GRANT_WRITE_URI_PERMISSION
            )
            link.prefs.saveTree?.let { old -> if (old != tree) releaseFolder(old) }
            link.prefs.saveTree = tree
        }
        tick++
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        enableEdgeToEdge()
        super.onCreate(savedInstanceState)
        link.start()
        LinkService.start(this)
        setContent {
            TetherTheme {
                when (screen) {
                    Screen.MAIN -> MainScreen()
                    Screen.APPS -> {
                        BackHandler { screen = Screen.MAIN }
                        AppsScreen()
                    }
                }
            }
        }
        handleIntent(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handleIntent(intent)
    }

    override fun onStart() {
        super.onStart()
        unobserve = link.observe { status = it }
    }

    override fun onStop() {
        unobserve?.invoke()
        super.onStop()
    }

    override fun onResume() {
        super.onResume()
        tick++
    }

    private fun handleIntent(intent: Intent?) {
        val data = intent?.data ?: return
        if (data.scheme == "tether") pair(data.toString())
    }

    private fun startScan() = scan.launch(
        ScanOptions()
            .setDesiredBarcodeFormats(ScanOptions.QR_CODE)
            .setPrompt("Scan the code from the tether panel widget or `tether pair`")
            .setBeepEnabled(false)
            .setCaptureActivity(PortraitCaptureActivity::class.java)
            .setOrientationLocked(true)
    )

    private fun pair(uri: String) {
        pairingNote = "Pairing…"
        link.pair(uri) { err ->
            if (err == null) {
                pairingNote = null
                toast("Paired with ${link.prefs.serverName}")
                LinkService.start(this)
            } else {
                pairingNote = "Pairing failed: $err"
            }
            tick++
        }
    }

    // ---- Main screen ----

    @OptIn(ExperimentalMaterial3Api::class)
    @Composable
    private fun MainScreen() {
        val scroll = TopAppBarDefaults.exitUntilCollapsedScrollBehavior()
        @Suppress("UNUSED_VARIABLE") val t = tick // recompose on resume
        val paired = link.prefs.server != null
        val missing = requirements()
        var confirmUnpair by remember { mutableStateOf(false) }

        Scaffold(
            modifier = Modifier.nestedScroll(scroll.nestedScrollConnection),
            topBar = { LargeTopAppBar(title = { Text("tether") }, scrollBehavior = scroll) },
        ) { padding ->
            LazyColumn(
                contentPadding = PaddingValues(
                    start = 16.dp, end = 16.dp,
                    top = padding.calculateTopPadding(), bottom = padding.calculateBottomPadding() + 24.dp,
                ),
                verticalArrangement = Arrangement.spacedBy(16.dp),
            ) {
                item { StatusCard() }
                if (missing.isNotEmpty()) item { SetupCard(missing) }
                if (paired) {
                    item { SettingsCard(onUnpair = { confirmUnpair = true }) }
                }
            }
        }

        if (confirmUnpair) {
            AlertDialog(
                onDismissRequest = { confirmUnpair = false },
                icon = { Icon(Icons.Rounded.LinkOff, null) },
                title = { Text("Unpair ${link.prefs.serverName}?") },
                text = { Text("This phone stops syncing until you pair again.") },
                confirmButton = {
                    TextButton(onClick = {
                        confirmUnpair = false
                        link.unpair()
                        stopService(Intent(this, LinkService::class.java))
                        tick++
                    }) { Text("Unpair") }
                },
                dismissButton = { TextButton(onClick = { confirmUnpair = false }) { Text("Cancel") } },
            )
        }
    }

    @Composable
    private fun StatusCard() {
        val desktop = link.prefs.serverName
        val c = MaterialTheme.colorScheme
        val (container, onContainer) = when (status) {
            Status.CONNECTED -> c.primaryContainer to c.onPrimaryContainer
            Status.REJECTED -> c.errorContainer to c.onErrorContainer
            Status.UNPAIRED -> c.secondaryContainer to c.onSecondaryContainer
            else -> c.surfaceContainerHigh to c.onSurface
        }
        val (icon, title, detail) = when (status) {
            Status.CONNECTED -> Triple(Icons.Rounded.Link, desktop, "Connected")
            Status.CONNECTING -> Triple(Icons.Rounded.Sync, desktop, "Connecting…")
            Status.OFFLINE -> Triple(Icons.Rounded.LinkOff, desktop, "Not reachable right now. tether reconnects on its own when it can.")
            Status.REJECTED -> Triple(Icons.Rounded.ErrorOutline, desktop, "$desktop no longer trusts this phone. Pair again to reconnect.")
            Status.UNPAIRED -> Triple(
                Icons.Rounded.QrCodeScanner, "Pair with your computer",
                "Open the tether panel widget, or run tether pair, then scan the code it shows.",
            )
        }

        Card(
            colors = CardDefaults.cardColors(containerColor = container, contentColor = onContainer),
            shape = RoundedCornerShape(28.dp),
        ) {
            Column(Modifier.padding(24.dp)) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Box(
                        Modifier.size(56.dp).background(onContainer.copy(alpha = 0.12f), CircleShape),
                        contentAlignment = Alignment.Center,
                    ) { Icon(icon, null, Modifier.size(28.dp)) }
                    Spacer(Modifier.width(16.dp))
                    Column {
                        Text(title, style = MaterialTheme.typography.titleLarge)
                        Text(
                            pairingNote ?: detail,
                            style = MaterialTheme.typography.bodyMedium,
                            color = onContainer.copy(alpha = 0.8f),
                        )
                    }
                }
                if (status == Status.CONNECTING || pairingNote == "Pairing…") {
                    Spacer(Modifier.height(16.dp))
                    LinearProgressIndicator(Modifier.fillMaxWidth())
                }
                Spacer(Modifier.height(20.dp))
                when (status) {
                    Status.UNPAIRED, Status.REJECTED -> Button(onClick = ::startScan) {
                        Icon(Icons.Rounded.QrCodeScanner, null, Modifier.size(18.dp))
                        Spacer(Modifier.width(8.dp))
                        Text("Scan QR code")
                    }
                    else -> FilledTonalButton(onClick = {
                        toast(if (Clip.sendCurrent(this@MainActivity)) "Clipboard sent" else "Clipboard is empty")
                    }) {
                        Icon(Icons.Rounded.ContentPaste, null, Modifier.size(18.dp))
                        Spacer(Modifier.width(8.dp))
                        Text("Send clipboard")
                    }
                }
            }
        }
    }

    private fun requirements(): List<Requirement> {
        val out = mutableListOf<Requirement>()
        val nm = getSystemService(NotificationManager::class.java)
        if (!nm.isNotificationListenerAccessGranted(ComponentName(this, NotifListener::class.java))) {
            out += Requirement(Icons.Rounded.Notifications, "Notification access", "To show your phone's notifications on the desktop") {
                startActivity(Intent(Settings.ACTION_NOTIFICATION_LISTENER_SETTINGS))
            }
        }
        if (Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            out += Requirement(Icons.Rounded.NotificationsActive, "Show notifications", "For transfer progress and received files") {
                askNotify.launch(Manifest.permission.POST_NOTIFICATIONS)
            }
        }
        if (!getSystemService(PowerManager::class.java).isIgnoringBatteryOptimizations(packageName)) {
            out += Requirement(Icons.Rounded.BatteryAlert, "Run in background", "Keeps the connection alive while the screen is off") {
                startActivity(Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:$packageName")))
            }
        }
        return out
    }

    @Composable
    private fun SetupCard(missing: List<Requirement>) {
        SectionCard("Finish setup") {
            for (r in missing) {
                ListItem(
                    headlineContent = { Text(r.title) },
                    supportingContent = { Text(r.why) },
                    leadingContent = { Icon(r.icon, null, tint = MaterialTheme.colorScheme.primary) },
                    trailingContent = { TextButton(onClick = r.grant) { Text("Allow") } },
                    colors = ListItemDefaults.colors(containerColor = Color.Transparent),
                )
            }
        }
    }

    @Composable
    private fun SettingsCard(onUnpair: () -> Unit) {
        val prefs = link.prefs
        val seen = prefs.seenApps()
        val muted = seen.keys.count(prefs::isMuted)
        val tree = prefs.saveTree

        SectionCard("Settings") {
            SettingRow(
                Icons.Rounded.Notifications, "Forwarded notifications",
                when {
                    seen.isEmpty() -> "All apps"
                    muted == 0 -> "All ${seen.size} apps"
                    else -> "${seen.size - muted} of ${seen.size} apps"
                },
            ) { screen = Screen.APPS }
            SettingRow(
                Icons.Rounded.Folder, "Received files",
                tree?.let(::folderLabel) ?: "Downloads",
                trailing = if (tree == null) null else {
                    {
                        IconButton(onClick = {
                            releaseFolder(tree)
                            prefs.saveTree = null
                            tick++
                        }) { Icon(Icons.Rounded.Restore, "Use Downloads") }
                    }
                },
            ) { pickFolder.launch(tree) }
            if (Build.VERSION.SDK_INT >= 33) {
                SettingRow(Icons.Rounded.Tune, "Quick Settings tile", "Send the clipboard from anywhere", onClick = ::addTile)
            }
            SettingRow(Icons.Rounded.QrCodeScanner, "Pair with another computer", "Replaces ${prefs.serverName}", onClick = ::startScan)
            SettingRow(Icons.Rounded.LinkOff, "Unpair", "Forget ${prefs.serverName}", danger = true, onClick = onUnpair)
        }
    }

    // ---- Forwarded apps screen ----

    @OptIn(ExperimentalMaterial3Api::class)
    @Composable
    private fun AppsScreen() {
        val prefs = link.prefs
        val apps = remember { prefs.seenApps().entries.sortedBy { it.value.lowercase() } }
        val enabled = remember { mutableStateMapOf<String, Boolean>().apply { apps.forEach { put(it.key, !prefs.isMuted(it.key)) } } }

        Scaffold(
            topBar = {
                TopAppBar(
                    title = { Text("Forwarded notifications") },
                    navigationIcon = {
                        IconButton(onClick = { screen = Screen.MAIN }) {
                            Icon(Icons.AutoMirrored.Rounded.ArrowBack, "Back")
                        }
                    },
                )
            },
        ) { padding ->
            if (apps.isEmpty()) {
                Box(Modifier.fillMaxSize().padding(padding).padding(32.dp), contentAlignment = Alignment.Center) {
                    Text(
                        "Apps show up here once they have posted a notification.",
                        style = MaterialTheme.typography.bodyLarge,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                return@Scaffold
            }
            LazyColumn(contentPadding = padding) {
                items(apps, key = { it.key }) { (pkg, label) ->
                    val on = enabled[pkg] ?: true
                    ListItem(
                        modifier = Modifier.clickable {
                            enabled[pkg] = !on
                            prefs.setMuted(pkg, on)
                        },
                        headlineContent = { Text(label) },
                        supportingContent = { Text(pkg, fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodySmall) },
                        leadingContent = { AppIcon(pkg) },
                        trailingContent = {
                            Switch(checked = on, onCheckedChange = {
                                enabled[pkg] = it
                                prefs.setMuted(pkg, !it)
                            })
                        },
                    )
                }
            }
        }
    }

    @Composable
    private fun AppIcon(pkg: String) {
        val bitmap = remember(pkg) {
            try {
                val d = packageManager.getApplicationIcon(pkg)
                val size = (40 * resources.displayMetrics.density).toInt()
                Bitmap.createBitmap(size, size, Bitmap.Config.ARGB_8888).also {
                    d.setBounds(0, 0, size, size)
                    d.draw(Canvas(it))
                }.asImageBitmap()
            } catch (_: PackageManager.NameNotFoundException) {
                null
            }
        }
        if (bitmap != null) {
            Image(bitmap, null, Modifier.size(40.dp))
        } else {
            Box(Modifier.size(40.dp).background(MaterialTheme.colorScheme.surfaceVariant, CircleShape))
        }
    }

    // ---- Helpers ----

    private fun addTile() {
        if (Build.VERSION.SDK_INT < 33) return
        getSystemService(StatusBarManager::class.java).requestAddTileService(
            ComponentName(this, ClipTile::class.java),
            getString(R.string.tile_label),
            Icon.createWithResource(this, R.drawable.ic_stat),
            mainExecutor,
        ) { result ->
            if (result == StatusBarManager.TILE_ADD_REQUEST_RESULT_TILE_ALREADY_ADDED) toast("The tile is already in Quick Settings")
        }
    }

    /** "primary:Documents/tether" -> "Documents/tether". */
    private fun folderLabel(tree: Uri): String =
        DocumentsContract.getTreeDocumentId(tree).substringAfter(':').ifEmpty { "Storage root" }

    private fun releaseFolder(tree: Uri) {
        try {
            contentResolver.releasePersistableUriPermission(
                tree, Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_GRANT_WRITE_URI_PERMISSION
            )
        } catch (_: SecurityException) {
        }
    }

    private fun toast(msg: String) = Toast.makeText(this, msg, Toast.LENGTH_SHORT).show()
}

@Composable
private fun SectionCard(title: String, content: @Composable () -> Unit) {
    Column {
        Text(
            title,
            style = MaterialTheme.typography.titleSmall,
            color = MaterialTheme.colorScheme.primary,
            modifier = Modifier.padding(start = 16.dp, bottom = 8.dp),
        )
        Card(
            colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerLow),
            shape = RoundedCornerShape(24.dp),
        ) {
            Column(Modifier.padding(vertical = 8.dp)) { content() }
        }
    }
}

@Composable
private fun SettingRow(
    icon: ImageVector,
    title: String,
    detail: String,
    danger: Boolean = false,
    trailing: (@Composable () -> Unit)? = null,
    onClick: () -> Unit,
) {
    val tint = if (danger) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant
    ListItem(
        modifier = Modifier.clickable(onClick = onClick),
        headlineContent = {
            Text(title, color = if (danger) MaterialTheme.colorScheme.error else Color.Unspecified)
        },
        supportingContent = { Text(detail) },
        leadingContent = { Icon(icon, null, tint = tint) },
        trailingContent = trailing,
        colors = ListItemDefaults.colors(containerColor = Color.Transparent),
    )
}
