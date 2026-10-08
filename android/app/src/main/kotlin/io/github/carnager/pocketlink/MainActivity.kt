package io.github.carnager.pocketlink

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
import androidx.compose.material.icons.rounded.Add
import androidx.compose.material.icons.rounded.BatteryAlert
import androidx.compose.material.icons.rounded.Call
import androidx.compose.material.icons.rounded.Close
import androidx.compose.material.icons.rounded.ContentPaste
import androidx.compose.material.icons.rounded.ErrorOutline
import androidx.compose.material.icons.rounded.Folder
import androidx.compose.material.icons.rounded.Link
import androidx.compose.material.icons.rounded.LinkOff
import androidx.compose.material.icons.rounded.MoreVert
import androidx.compose.material.icons.rounded.Notifications
import androidx.compose.material.icons.rounded.NotificationsActive
import androidx.compose.material.icons.rounded.QrCodeScanner
import androidx.compose.material.icons.rounded.Restore
import androidx.compose.material.icons.rounded.Search
import androidx.compose.material.icons.rounded.Sync
import androidx.compose.material.icons.rounded.Tune
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.OutlinedTextField
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
import androidx.compose.runtime.produceState
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.input.nestedscroll.nestedScroll
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.journeyapps.barcodescanner.CaptureActivity
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanOptions

/** The scanner library's capture screen, locked to portrait in the manifest. */
class PortraitCaptureActivity : CaptureActivity()

private enum class Screen { MAIN, APPS }

private class Requirement(
    val icon: ImageVector,
    val title: String,
    val why: String,
    /** Extra help, with a button for it, for when granting isn't straightforward. */
    val hint: String? = null,
    val hintAction: Pair<String, () -> Unit>? = null,
    val grant: () -> Unit,
)

class MainActivity : ComponentActivity() {
    private val links by lazy { Links.get(this) }

    private var pairingNote by mutableStateOf<String?>(null)
    private var screen by mutableStateOf(Screen.MAIN)
    // Bumped on resume and after changes, so permission and settings rows re-read their state.
    private var tick by mutableIntStateOf(0)
    private var unobserve: (() -> Unit)? = null

    private val scan = registerForActivityResult(ScanContract()) { r -> r.contents?.let(::pair) }
    private val askNotify = registerForActivityResult(ActivityResultContracts.RequestPermission()) { tick++ }
    private val askCalls = registerForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { granted ->
        if (granted[Manifest.permission.READ_PHONE_STATE] != true) {
            // After a "don't ask again", only the app settings can grant it.
            startActivity(Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:$packageName")))
        }
        tick++
    }
    private val pickFolder = registerForActivityResult(ActivityResultContracts.OpenDocumentTree()) { tree ->
        if (tree != null) {
            // Keep access across reboots.
            contentResolver.takePersistableUriPermission(
                tree, Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_GRANT_WRITE_URI_PERMISSION
            )
            links.prefs.saveTree?.let { old -> if (old != tree) releaseFolder(old) }
            links.prefs.saveTree = tree
        }
        tick++
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        enableEdgeToEdge()
        super.onCreate(savedInstanceState)
        links.start()
        LinkService.start(this)
        setContent {
            PocketlinkTheme {
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
        unobserve = links.observe { tick++ }
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
        if (data.scheme == "pocketlink") pair(data.toString())
    }

    private fun startScan() = scan.launch(
        ScanOptions()
            .setDesiredBarcodeFormats(ScanOptions.QR_CODE)
            .setPrompt("Scan the code from the pocketlink panel widget or `pocketlink pair`")
            .setBeepEnabled(false)
            .setCaptureActivity(PortraitCaptureActivity::class.java)
            .setOrientationLocked(true)
    )

    private fun pair(uri: String) {
        pairingNote = "Pairing…"
        links.pair(uri) { err ->
            if (err == null) {
                pairingNote = null
                toast("Paired")
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
        @Suppress("UNUSED_VARIABLE") val t = tick // recompose on resume and link changes
        val all = links.all()
        val missing = requirements()
        var confirmUnpair by remember { mutableStateOf<Link?>(null) }

        Scaffold(
            modifier = Modifier.nestedScroll(scroll.nestedScrollConnection),
            topBar = { LargeTopAppBar(title = { Text("pocketlink") }, scrollBehavior = scroll) },
        ) { padding ->
            LazyColumn(
                contentPadding = PaddingValues(
                    start = 16.dp, end = 16.dp,
                    top = padding.calculateTopPadding(), bottom = padding.calculateBottomPadding() + 24.dp,
                ),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                if (all.isEmpty()) {
                    item { PairCard() }
                } else {
                    items(all, key = { it.id }) { ComputerCard(it, onUnpair = { confirmUnpair = it }) }
                    item {
                        pairingNote?.let { Text(it, Modifier.padding(horizontal = 8.dp), style = MaterialTheme.typography.bodyMedium) }
                        FilledTonalButton(
                            onClick = { toast(if (Clip.sendCurrent(this@MainActivity)) "Clipboard sent" else "Clipboard is empty") },
                            enabled = all.any { it.status == Status.CONNECTED },
                            modifier = Modifier.fillMaxWidth(),
                        ) {
                            Icon(Icons.Rounded.ContentPaste, null, Modifier.size(18.dp))
                            Spacer(Modifier.width(8.dp))
                            Text("Send clipboard")
                        }
                    }
                }
                if (missing.isNotEmpty()) item { SetupCard(missing) }
                if (all.isNotEmpty()) item { SettingsCard() }
            }
        }

        confirmUnpair?.let { link ->
            AlertDialog(
                onDismissRequest = { confirmUnpair = null },
                icon = { Icon(Icons.Rounded.LinkOff, null) },
                title = { Text("Unpair ${link.name}?") },
                text = { Text("This phone stops syncing with ${link.name} until you pair again.") },
                confirmButton = {
                    TextButton(onClick = {
                        confirmUnpair = null
                        links.unpair(link.id)
                        if (!links.isPaired) stopService(Intent(this, LinkService::class.java))
                    }) { Text("Unpair") }
                },
                dismissButton = { TextButton(onClick = { confirmUnpair = null }) { Text("Cancel") } },
            )
        }
    }

    @Composable
    private fun PairCard() {
        val c = MaterialTheme.colorScheme
        Card(
            colors = CardDefaults.cardColors(containerColor = c.secondaryContainer, contentColor = c.onSecondaryContainer),
            shape = RoundedCornerShape(28.dp),
        ) {
            Column(Modifier.padding(24.dp)) {
                StatusHeader(
                    Icons.Rounded.QrCodeScanner, c.onSecondaryContainer, "Pair with your computer",
                    pairingNote ?: "Open the pocketlink panel widget, or run pocketlink pair, then scan the code it shows.",
                )
                if (pairingNote == "Pairing…") {
                    Spacer(Modifier.height(16.dp))
                    LinearProgressIndicator(Modifier.fillMaxWidth())
                }
                Spacer(Modifier.height(20.dp))
                Button(onClick = ::startScan) {
                    Icon(Icons.Rounded.QrCodeScanner, null, Modifier.size(18.dp))
                    Spacer(Modifier.width(8.dp))
                    Text("Scan QR code")
                }
            }
        }
    }

    @Composable
    private fun ComputerCard(link: Link, onUnpair: () -> Unit) {
        val c = MaterialTheme.colorScheme
        val status = link.status
        val (container, onContainer) = when (status) {
            Status.CONNECTED -> c.primaryContainer to c.onPrimaryContainer
            Status.REJECTED -> c.errorContainer to c.onErrorContainer
            else -> c.surfaceContainerHigh to c.onSurface
        }
        val icon = when (status) {
            Status.CONNECTED -> Icons.Rounded.Link
            Status.CONNECTING -> Icons.Rounded.Sync
            Status.OFFLINE -> Icons.Rounded.LinkOff
            Status.REJECTED -> Icons.Rounded.ErrorOutline
        }
        Card(
            colors = CardDefaults.cardColors(containerColor = container, contentColor = onContainer),
            shape = RoundedCornerShape(28.dp),
        ) {
            Column(Modifier.padding(start = 20.dp, top = 20.dp, bottom = 20.dp, end = 8.dp)) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Box(Modifier.weight(1f)) {
                        StatusHeader(icon, onContainer, link.name, status.describe())
                    }
                    IconButton(onClick = onUnpair) { Icon(Icons.Rounded.LinkOff, "Unpair ${link.name}") }
                }
                if (status == Status.CONNECTING) {
                    Spacer(Modifier.height(12.dp))
                    LinearProgressIndicator(Modifier.fillMaxWidth().padding(end = 12.dp))
                }
                if (status == Status.REJECTED) {
                    Spacer(Modifier.height(12.dp))
                    Button(onClick = ::startScan) { Text("Pair again") }
                }
            }
        }
    }

    @Composable
    private fun StatusHeader(icon: ImageVector, onContainer: Color, title: String, detail: String) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Box(
                Modifier.size(52.dp).background(onContainer.copy(alpha = 0.12f), CircleShape),
                contentAlignment = Alignment.Center,
            ) { Icon(icon, null, Modifier.size(26.dp)) }
            Spacer(Modifier.width(16.dp))
            Column {
                Text(title, style = MaterialTheme.typography.titleLarge)
                Text(detail, style = MaterialTheme.typography.bodyMedium, color = onContainer.copy(alpha = 0.8f))
            }
        }
    }

    private fun requirements(): List<Requirement> {
        val out = mutableListOf<Requirement>()
        val nm = getSystemService(NotificationManager::class.java)
        if (!nm.isNotificationListenerAccessGranted(ComponentName(this, NotifListener::class.java))) {
            out += Requirement(
                Icons.Rounded.Notifications, "Notification access", "To show your phone's notifications on the desktop",
                // Android blocks this for apps installed from a browser download.
                hint = "Switch greyed out? Open App info, tap ⋮ and choose “Allow restricted settings”, then try again.",
                hintAction = "App info" to {
                    startActivity(Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:$packageName")))
                },
            ) {
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
                    supportingContent = {
                        Column {
                            Text(r.why)
                            r.hint?.let {
                                Spacer(Modifier.height(4.dp))
                                Text(it, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                            }
                            r.hintAction?.let { (label, action) ->
                                TextButton(onClick = action, contentPadding = PaddingValues(0.dp)) { Text(label) }
                            }
                        }
                    },
                    leadingContent = { Icon(r.icon, null, tint = MaterialTheme.colorScheme.primary) },
                    trailingContent = { TextButton(onClick = r.grant) { Text("Allow") } },
                    colors = ListItemDefaults.colors(containerColor = Color.Transparent),
                )
            }
        }
    }

    @Composable
    private fun SettingsCard() {
        val prefs = links.prefs
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
            val callsOn = Calls.enabled(this@MainActivity)
            SettingRow(
                Icons.Rounded.Call, "Phone calls",
                if (callsOn) "Shown on your computers, which pause or quieten media" else "Off. Tap to show calls on your computers",
            ) {
                if (callsOn) {
                    startActivity(Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:$packageName")))
                } else {
                    askCalls.launch(Calls.permissions)
                }
            }
            if (Build.VERSION.SDK_INT >= 33) {
                SettingRow(Icons.Rounded.Tune, "Quick Settings tile", "Send the clipboard from anywhere", onClick = ::addTile)
            }
            SettingRow(Icons.Rounded.Add, "Add computer", "Pair with another computer", onClick = ::startScan)
        }
    }

    // ---- Forwarded apps screen ----

    private class AppEntry(val pkg: String, val label: String, val notified: Boolean)

    /** Launchable apps plus anything that has posted a notification, by label. */
    private fun loadApps(): List<AppEntry> {
        val pm = packageManager
        val launcher = Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER)
        @Suppress("DEPRECATION")
        val activities = if (Build.VERSION.SDK_INT >= 33) {
            pm.queryIntentActivities(launcher, PackageManager.ResolveInfoFlags.of(0))
        } else {
            pm.queryIntentActivities(launcher, 0)
        }
        val labels = LinkedHashMap<String, String>()
        for (ri in activities) labels.putIfAbsent(ri.activityInfo.packageName, ri.loadLabel(pm).toString())
        val seen = links.prefs.seenApps()
        for ((pkg, label) in seen) labels.putIfAbsent(pkg, label)
        labels.remove(packageName)
        return labels.map { (pkg, label) -> AppEntry(pkg, label, pkg in seen) }.sortedBy { it.label.lowercase() }
    }

    @OptIn(ExperimentalMaterial3Api::class)
    @Composable
    private fun AppsScreen() {
        val prefs = links.prefs
        val apps by produceState<List<AppEntry>?>(null) { value = withContext(Dispatchers.IO) { loadApps() } }
        var query by rememberSaveable { mutableStateOf("") }
        var menuOpen by remember { mutableStateOf(false) }
        // Mirrors prefs so switches update immediately.
        val forwarded = remember { mutableStateMapOf<String, Boolean>() }
        fun isOn(pkg: String) = forwarded[pkg] ?: !prefs.isMuted(pkg)
        fun setOn(pkgs: List<String>, on: Boolean) {
            pkgs.forEach { forwarded[it] = on }
            prefs.setMuted(pkgs, !on)
        }

        val shown = apps.orEmpty().filter {
            query.isBlank() || it.label.contains(query, ignoreCase = true) || it.pkg.contains(query, ignoreCase = true)
        }
        val (notified, others) = shown.partition { it.notified }

        Scaffold(
            topBar = {
                TopAppBar(
                    title = { Text("Forwarded notifications") },
                    navigationIcon = {
                        IconButton(onClick = { screen = Screen.MAIN }) {
                            Icon(Icons.AutoMirrored.Rounded.ArrowBack, "Back")
                        }
                    },
                    actions = {
                        IconButton(onClick = { menuOpen = true }) { Icon(Icons.Rounded.MoreVert, "More") }
                        DropdownMenu(expanded = menuOpen, onDismissRequest = { menuOpen = false }) {
                            DropdownMenuItem(
                                text = { Text(if (query.isBlank()) "Forward all" else "Forward all shown") },
                                onClick = { setOn(shown.map { it.pkg }, true); menuOpen = false },
                            )
                            DropdownMenuItem(
                                text = { Text(if (query.isBlank()) "Forward none" else "Forward none shown") },
                                onClick = { setOn(shown.map { it.pkg }, false); menuOpen = false },
                            )
                        }
                    },
                )
            },
        ) { padding ->
            Column(Modifier.padding(top = padding.calculateTopPadding()).fillMaxSize()) {
                OutlinedTextField(
                    value = query,
                    onValueChange = { query = it },
                    modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp),
                    placeholder = { Text("Search apps") },
                    leadingIcon = { Icon(Icons.Rounded.Search, null) },
                    trailingIcon = {
                        if (query.isNotEmpty()) IconButton(onClick = { query = "" }) { Icon(Icons.Rounded.Close, "Clear") }
                    },
                    singleLine = true,
                    shape = RoundedCornerShape(28.dp),
                )
                when {
                    apps == null -> Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) { CircularProgressIndicator() }
                    shown.isEmpty() -> Box(Modifier.fillMaxSize().padding(32.dp), contentAlignment = Alignment.Center) {
                        Text("No apps match “$query”", color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                    else -> LazyColumn(contentPadding = PaddingValues(bottom = padding.calculateBottomPadding() + 16.dp)) {
                        if (notified.isNotEmpty()) {
                            item(key = "h-notified") { ListHeader("Recently sent notifications") }
                            items(notified, key = { "n-" + it.pkg }) { AppRow(it, isOn(it.pkg)) { on -> setOn(listOf(it.pkg), on) } }
                        }
                        if (others.isNotEmpty()) {
                            item(key = "h-others") { ListHeader("Other apps") }
                            items(others, key = { "o-" + it.pkg }) { AppRow(it, isOn(it.pkg)) { on -> setOn(listOf(it.pkg), on) } }
                        }
                    }
                }
            }
        }
    }

    @Composable
    private fun AppRow(app: AppEntry, on: Boolean, onChange: (Boolean) -> Unit) {
        ListItem(
            modifier = Modifier.clickable { onChange(!on) },
            headlineContent = { Text(app.label) },
            supportingContent = {
                Text(app.pkg, style = MaterialTheme.typography.bodySmall, maxLines = 1, overflow = TextOverflow.Ellipsis)
            },
            leadingContent = { AppIcon(app.pkg) },
            trailingContent = { Switch(checked = on, onCheckedChange = onChange) },
        )
    }

    @Composable
    private fun ListHeader(text: String) {
        Text(
            text,
            style = MaterialTheme.typography.titleSmall,
            color = MaterialTheme.colorScheme.primary,
            modifier = Modifier.padding(start = 16.dp, top = 16.dp, bottom = 4.dp),
        )
    }

    @Composable
    private fun AppIcon(pkg: String) {
        val bitmap by produceState<ImageBitmap?>(null, pkg) {
            value = withContext(Dispatchers.IO) {
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
        }
        val b = bitmap
        if (b != null) {
            Image(b, null, Modifier.size(40.dp))
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

    /** "primary:Documents/pocketlink" -> "Documents/pocketlink". */
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
