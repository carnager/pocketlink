package io.github.carnager.tether

import android.content.Context
import android.net.Uri
import java.io.File
import java.util.UUID
import org.json.JSONArray
import org.json.JSONObject

/** Wire format helpers; see internal/proto in the Go code. */
object Proto {
    const val VERSION = 1

    fun frame(type: String, body: JSONObject, acked: Boolean = true): JSONObject =
        JSONObject().put("v", VERSION).put("type", type).put("body", body).apply {
            if (acked) put("id", UUID.randomUUID().toString().replace("-", ""))
        }

    fun ack(id: String): JSONObject = JSONObject().put("v", VERSION).put("type", "ack").put("ref", id)
}

data class Server(val name: String, val fp: String, val addrs: List<String>, val lastAddr: String? = null) {
    val id get() = fp

    fun toJson(): JSONObject = JSONObject()
        .put("name", name).put("fp", fp).put("addrs", JSONArray(addrs)).put("last_addr", lastAddr)

    companion object {
        fun fromJson(o: JSONObject) = Server(
            name = o.optString("name", "desktop"),
            fp = o.getString("fp"),
            addrs = o.optJSONArray("addrs")?.let { a -> List(a.length()) { a.getString(it) } }.orEmpty(),
            lastAddr = o.optString("last_addr").ifEmpty { null },
        )
    }
}

/** A `tether://pair?...` link from the desktop's QR code. */
data class Offer(val name: String, val fp: String, val token: String, val addrs: List<String>) {
    companion object {
        fun parse(s: String): Offer? {
            val u = Uri.parse(s.trim())
            if (u.scheme != "tether" || u.host != "pair") return null
            val fp = u.getQueryParameter("fp") ?: return null
            val token = u.getQueryParameter("t") ?: return null
            val addrs = u.getQueryParameter("a")?.split(",")?.filter { it.isNotBlank() }.orEmpty()
            if (addrs.isEmpty()) return null
            return Offer(u.getQueryParameter("n") ?: "desktop", fp, token, addrs)
        }
    }
}

class Prefs(ctx: Context) {
    private val sp = ctx.getSharedPreferences("tether", Context.MODE_PRIVATE)

    init {
        migrateSingleServer()
    }

    /** Paired computers, in pairing order. */
    @Synchronized
    fun servers(): List<Server> {
        val a = JSONArray(sp.getString("servers", "[]")!!)
        return List(a.length()) { Server.fromJson(a.getJSONObject(it)) }
    }

    fun server(id: String): Server? = servers().find { it.id == id }

    /** Adds [s], or replaces the entry for the same computer. */
    @Synchronized
    fun putServer(s: Server) {
        val list = servers().toMutableList()
        val i = list.indexOfFirst { it.id == s.id }
        if (i >= 0) list[i] = s else list.add(s)
        save(list)
    }

    @Synchronized
    fun updateServer(id: String, f: (Server) -> Server) {
        save(servers().map { if (it.id == id) f(it) else it })
    }

    @Synchronized
    fun removeServer(id: String) {
        save(servers().filter { it.id != id })
    }

    private fun save(list: List<Server>) {
        sp.edit().putString("servers", JSONArray(list.map { it.toJson() }).toString()).apply()
    }

    /** Versions before multi-computer support stored one server in flat keys. */
    private fun migrateSingleServer() {
        val fp = sp.getString("fp", null) ?: return
        val addrs = sp.getString("addrs", "")!!.split(",").filter { it.isNotBlank() }
        val old = Server(sp.getString("name", "desktop")!!, fp, addrs, sp.getString("last_addr", null))
        if (servers().none { it.id == fp }) save(servers() + old)
        sp.edit().remove("fp").remove("addrs").remove("name").remove("last_addr").apply()
    }

    /** Folder picked for received files (a document tree URI), or null for Downloads. */
    var saveTree: Uri?
        get() = sp.getString("save_tree", null)?.let(Uri::parse)
        set(v) = sp.edit().putString("save_tree", v?.toString()).apply()

    /** Apps whose notifications are not forwarded. */
    fun isMuted(pkg: String) = pkg in sp.getStringSet("muted", emptySet())!!

    fun setMuted(pkg: String, muted: Boolean) = setMuted(listOf(pkg), muted)

    fun setMuted(pkgs: Collection<String>, muted: Boolean) {
        val set = sp.getStringSet("muted", emptySet())!!.toMutableSet()
        if (muted) set.addAll(pkgs) else set.removeAll(pkgs.toSet())
        sp.edit().putStringSet("muted", set).apply()
    }

    /** Apps that have posted notifications, package -> label, for the filter screen. */
    fun seenApps(): Map<String, String> {
        val o = JSONObject(sp.getString("seen_apps", "{}")!!)
        return o.keys().asSequence().associateWith { o.getString(it) }
    }

    fun addSeenApp(pkg: String, label: String) {
        val o = JSONObject(sp.getString("seen_apps", "{}")!!)
        if (o.optString(pkg) == label) return
        sp.edit().putString("seen_apps", o.put(pkg, label).toString()).apply()
    }
}

/**
 * A JSON list persisted to a file, rewritten atomically on every change.
 * Only touched from one thread per instance.
 */
open class JsonList(private val file: File) {
    protected val items: MutableList<JSONObject> = load()

    private fun load(): MutableList<JSONObject> = try {
        val arr = JSONArray(file.readText())
        MutableList(arr.length()) { arr.getJSONObject(it) }
    } catch (_: Exception) {
        mutableListOf()
    }

    protected fun save() {
        file.parentFile?.mkdirs()
        val tmp = File(file.path + ".tmp")
        tmp.writeText(JSONArray(items).toString())
        tmp.renameTo(file)
    }

    fun all(): List<JSONObject> = items.toList()

    fun clear() {
        items.clear()
        save()
    }
}

/**
 * Frames waiting for the desktop's ack. Mirrors the desktop's queue: frames
 * survive disconnects and app restarts, and are resent until acked.
 */
class Outbox(file: File) : JsonList(file) {
    fun push(f: JSONObject) {
        val type = f.getString("type")
        val key = f.optJSONObject("body")?.optString("key")
        when (type) {
            // Only the newest clipboard and call state matter.
            "clip.set", "call.state" -> items.removeAll { it.getString("type") == type }
            // A newer version or removal supersedes a pending notification.
            "notif.posted", "notif.removed" -> items.removeAll {
                it.getString("type") == "notif.posted" && it.optJSONObject("body")?.optString("key") == key
            }
        }
        items.add(f)
        while (items.size > MAX) items.removeAt(0)
        save()
    }

    fun ack(id: String) {
        if (items.removeAll { it.optString("id") == id }) save()
    }

    /** Pending frames, minus notifications too old to be worth showing. */
    fun pending(now: Long): List<JSONObject> {
        val stale = items.filter {
            it.getString("type") == "notif.posted" && now - it.getJSONObject("body").optLong("time") > STALE_MS
        }
        if (stale.isNotEmpty()) {
            items.removeAll(stale)
            save()
        }
        return items.toList()
    }

    private companion object {
        const val MAX = 200
        const val STALE_MS = 60 * 60 * 1000L
    }
}

/** Records of transfers that must survive app restarts, keyed by "id". */
class TransferList(file: File) : JsonList(file) {
    fun add(o: JSONObject) {
        items.add(o)
        save()
    }

    fun remove(id: String) {
        if (items.removeAll { it.getString("id") == id }) save()
    }

    fun contains(id: String) = items.any { it.getString("id") == id }
}
