package io.github.carnager.pocketlink

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.Uri
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.util.Log
import java.io.File
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import org.json.JSONObject

private const val TAG = "pocketlink"

enum class Status { CONNECTING, CONNECTED, OFFLINE, REJECTED }

fun Status.describe(): String = when (this) {
    Status.CONNECTING -> "Connecting…"
    Status.CONNECTED -> "Connected"
    Status.OFFLINE -> "Not reachable right now"
    Status.REJECTED -> "No longer trusts this phone. Pair again."
}

/**
 * Runs [task] on this executor. An exception would otherwise vanish inside
 * the executor and could leave a connection without a retry scheduled.
 */
private fun ScheduledExecutorService.guarded(onError: () -> Unit = {}, task: () -> Unit) = execute {
    try {
        task()
    } catch (e: Exception) {
        Log.e(TAG, "task failed", e)
        onError()
    }
}

/**
 * All paired computers. Each has its own [Link]; things that concern every
 * computer (notifications, the clipboard) are sent to all of them.
 */
class Links private constructor(private val ctx: Context) {
    val prefs = Prefs(ctx)

    private val links = LinkedHashMap<String, Link>()
    private val main = Handler(Looper.getMainLooper())
    private val observers = CopyOnWriteArrayList<() -> Unit>()
    private val pairExec = Executors.newSingleThreadScheduledExecutor { Thread(it, "pocketlink-pair") }
    private var started = false

    init {
        for (s in prefs.servers()) links[s.id] = Link(ctx, s.id, this)
    }

    @Synchronized
    fun all(): List<Link> = links.values.toList()

    @Synchronized
    fun get(id: String): Link? = links[id]

    val isPaired get() = all().isNotEmpty()

    /** Calls [f] on the main thread now and whenever any computer's state changes. */
    fun observe(f: () -> Unit): () -> Unit {
        observers.add(f)
        main.post(f)
        return { observers.remove(f) }
    }

    internal fun changed() = main.post { observers.forEach { it() } }

    internal fun renamed() {
        ShareTargets.update(ctx, all())
        changed()
    }

    @Synchronized
    fun start() {
        if (started) return
        started = true
        val cm = ctx.getSystemService(ConnectivityManager::class.java)
        val initial = cm.activeNetwork
        cm.registerDefaultNetworkCallback(object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(n: Network) = all().forEach { it.onNetwork(n) }
            override fun onLost(n: Network) = all().forEach { it.onNetworkLost(n) }
        })
        all().forEach { it.start(initial) }
        ShareTargets.update(ctx, all())
    }

    /** Queues a frame for every paired computer. */
    fun send(type: String, body: JSONObject) = all().forEach { it.send(type, body) }

    /** Pairs using a QR link. [done] gets null on success or an error message. */
    fun pair(uri: String, done: (String?) -> Unit) = pairExec.guarded({ main.post { done("Internal error") } }) {
        val offer = Offer.parse(uri)
        if (offer == null) {
            main.post { done("Not a pocketlink pairing link") }
            return@guarded
        }
        pairAt(offer, 0, mutableListOf(), done)
    }

    fun unpair(id: String) {
        val link = synchronized(this) { links.remove(id) } ?: return
        prefs.removeServer(id)
        link.close()
        ShareTargets.update(ctx, all())
        changed()
    }

    private fun pairAt(offer: Offer, i: Int, errors: MutableList<String>, done: (String?) -> Unit) {
        if (i >= offer.addrs.size) {
            main.post { done(errors.joinToString("\n")) }
            return
        }
        val addr = offer.addrs[i]
        var finished = false // only touched on pairExec
        val url = "wss://$addr/ws?pair=${Uri.encode(offer.token)}&name=${Uri.encode(deviceName(ctx))}"
        Log.i(TAG, "pairing via $addr")

        fun next(error: String) {
            if (finished) return
            finished = true
            Log.w(TAG, "pairing via $addr failed: $error")
            errors.add("$addr: $error")
            pairAt(offer, i + 1, errors, done)
        }

        val ws = Identity.client(offer.fp).newWebSocket(Request.Builder().url(url).build(), object : WebSocketListener() {
            // The desktop sends hello only once it has accepted our certificate.
            override fun onMessage(webSocket: WebSocket, text: String) = pairExec.guarded {
                if (finished) return@guarded
                finished = true
                webSocket.close(1000, null)
                paired(Server(offer.name, offer.fp, offer.addrs, addr))
                main.post { done(null) }
            }

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) = pairExec.guarded {
                next(if (response?.code == 403) "pairing code expired or already used" else (t.message ?: t.toString()))
            }
        })
        pairExec.schedule({
            pairExec.guarded {
                if (!finished) {
                    ws.cancel()
                    next("timed out")
                }
            }
        }, PAIR_TIMEOUT_S, TimeUnit.SECONDS)
    }

    private fun paired(server: Server) {
        Log.i(TAG, "paired with ${server.name}")
        prefs.putServer(server)
        // Pairing again with a known computer replaces its old connection.
        val old = synchronized(this) { links.remove(server.id) }
        old?.close(keepData = true)
        val link = Link(ctx, server.id, this)
        synchronized(this) { links[server.id] = link }
        if (started) link.start(ctx.getSystemService(ConnectivityManager::class.java).activeNetwork)
        ShareTargets.update(ctx, all())
        changed()
    }

    companion object {
        private const val PAIR_TIMEOUT_S = 15L

        @Volatile
        private var instance: Links? = null

        fun get(ctx: Context): Links = instance ?: synchronized(this) {
            instance ?: Links(ctx.applicationContext).also { instance = it }
        }

        fun deviceName(ctx: Context): String =
            Settings.Global.getString(ctx.contentResolver, Settings.Global.DEVICE_NAME) ?: Build.MODEL
    }
}

/**
 * The connection to one computer. The phone always dials out; the
 * connection is expected to drop, and nothing depends on it staying up:
 * frames wait in the outbox until the computer acks them.
 *
 * Reconnects are driven by network changes rather than polling, with an
 * exponential backoff as a fallback. All mutable state is confined to [exec].
 */
class Link internal constructor(private val ctx: Context, val id: String, private val links: Links) {
    private val dir = File(ctx.filesDir, "servers/${id.take(16)}")
    val transfers = Transfers(ctx, this, dir, File(ctx.cacheDir, "servers/${id.take(16)}"))
    val remote = Remote(ctx, this)

    private val exec = Executors.newSingleThreadScheduledExecutor { Thread(it, "pocketlink-link-${id.take(6)}") }
    private val outbox = Outbox(File(dir, "outbox.json"))

    private var closed = false
    private var ws: WebSocket? = null
    private var generation = 0 // bumps whenever a socket is abandoned; stale callbacks are ignored
    private var connected = false
    private var connecting = false
    private var backoff = MIN_BACKOFF
    private var retry: ScheduledFuture<*>? = null
    private var network: Network? = null
    private val sent = HashSet<String>() // outbox IDs written on the current connection
    private val seen = object : LinkedHashMap<String, Unit>() {
        override fun removeEldestEntry(eldest: MutableMap.MutableEntry<String, Unit>?) = size > 512
    }

    val server: Server get() = links.prefs.server(id) ?: Server("computer", id, emptyList())
    val name: String get() = server.name

    /** Base URL and HTTP client for file transfers while connected. */
    @Volatile
    var target: Pair<String, OkHttpClient>? = null
        private set

    @Volatile
    var status: Status = Status.OFFLINE
        private set

    private fun run(task: () -> Unit) = exec.guarded({ if (!connected && !connecting) scheduleRetry() }, task)

    internal fun start(initial: Network?) = run {
        network = initial
        connect()
    }

    /** Queues a frame for this computer. */
    fun send(type: String, body: JSONObject) = run {
        outbox.push(Proto.frame(type, body))
        flush()
    }

    /**
     * Sends a frame only if connected right now, without queueing or acks.
     * For commands that would do the wrong thing if delivered late.
     */
    fun sendLive(type: String, body: JSONObject) = run {
        if (connected) ws?.send(Proto.frame(type, body, acked = false).toString())
    }

    /** Stops this connection; deletes its queued data unless [keepData]. */
    internal fun close(keepData: Boolean = false) = run {
        closed = true
        retry?.cancel(false)
        drop()
        if (!keepData) {
            outbox.clear()
            transfers.reset()
            dir.deleteRecursively()
        }
        exec.shutdown()
    }

    internal fun onNetwork(n: Network) = run {
        if (n == network) return@run
        // The default network changed (e.g. Wi-Fi to mobile). A socket on the
        // old network is likely dead even if it doesn't know it yet.
        network = n
        drop()
        backoff = MIN_BACKOFF
        connect()
    }

    internal fun onNetworkLost(n: Network) = run {
        if (n == network) {
            network = null
            drop()
        }
    }

    private fun connect() {
        retry?.cancel(false)
        retry = null
        if (closed || connected || connecting) return
        val s = server
        connecting = true
        if (status != Status.REJECTED) setStatus(Status.CONNECTING)
        tryAddr(s, s.addrs.sortedByDescending { it == s.lastAddr }, 0)
    }

    private fun tryAddr(server: Server, addrs: List<String>, i: Int) {
        if (i >= addrs.size) {
            connecting = false
            if (status != Status.REJECTED) setStatus(Status.OFFLINE)
            scheduleRetry()
            return
        }
        val addr = addrs[i]
        val gen = ++generation
        val client = Identity.client(server.fp)
        ws = client.newWebSocket(Request.Builder().url("wss://$addr/ws").build(), object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) = run {
                if (gen == generation) opened(webSocket, addr, client)
            }

            override fun onMessage(webSocket: WebSocket, text: String) = run {
                if (gen == generation) received(webSocket, text)
            }

            override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                webSocket.close(1000, null)
            }

            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) = run {
                if (gen == generation) lost(server, addrs, i, null, null)
            }

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) = run {
                if (gen == generation) lost(server, addrs, i, t, response?.code)
            }
        })
    }

    private fun opened(socket: WebSocket, addr: String, client: OkHttpClient) {
        connecting = false
        connected = true
        backoff = MIN_BACKOFF
        links.prefs.updateServer(id) { it.copy(lastAddr = addr) }
        target = "https://$addr" to client
        sent.clear()
        val hello = JSONObject().put("name", Links.deviceName(ctx)).put("platform", "android")
        socket.send(Proto.frame("hello", hello, acked = false).toString())
        Log.i(TAG, "connected to $name at $addr")
        setStatus(Status.CONNECTED)
        flush()
        transfers.kick()
    }

    private fun lost(server: Server, addrs: List<String>, i: Int, t: Throwable?, code: Int?) {
        ws = null
        if (connected) {
            Log.i(TAG, "disconnected from $name: ${t?.message}")
            connected = false
            target = null
            remote.clear()
            setStatus(Status.OFFLINE)
            backoff = MIN_BACKOFF
            scheduleRetry()
            return
        }
        if (code == 403) {
            // We're not in the computer's trust store (anymore).
            Log.w(TAG, "$name rejected our certificate")
            connecting = false
            setStatus(Status.REJECTED)
            backoff = MAX_BACKOFF
            scheduleRetry()
            return
        }
        Log.d(TAG, "${addrs[i]} failed: ${t?.message}")
        tryAddr(server, addrs, i + 1)
    }

    private fun received(socket: WebSocket, text: String) {
        val f = try {
            JSONObject(text)
        } catch (e: Exception) {
            Log.w(TAG, "malformed frame", e)
            return
        }
        val frameId = f.optString("id")
        val body = f.optJSONObject("body") ?: JSONObject()
        when (val type = f.optString("type")) {
            "ack" -> {
                val ref = f.optString("ref")
                outbox.ack(ref)
                sent.remove(ref)
            }
            "hello" -> body.optString("name").takeIf { it.isNotEmpty() && it != name }?.let { n ->
                links.prefs.updateServer(id) { it.copy(name = n) }
                links.renamed()
            }
            else -> {
                // Retransmitted frames are acked again but handled only once.
                if (frameId.isEmpty() || seen.put(frameId, Unit) == null) handle(type, body)
                if (frameId.isNotEmpty()) socket.send(Proto.ack(frameId).toString())
            }
        }
    }

    private fun handle(type: String, body: JSONObject) {
        when (type) {
            "clip.set" -> Clip.set(ctx, body.optString("text"))
            "clip.image" -> Clip.fetchImage(ctx, this, body)
            "file.offer" -> transfers.offered(body)
            "media.state" -> remote.update(body)
            "call.mute" -> Calls.muteRinger(ctx)
            else -> Log.d(TAG, "ignoring $type")
        }
    }

    private fun flush() {
        val socket = ws ?: return
        if (!connected) return
        for (f in outbox.pending(System.currentTimeMillis())) {
            val fid = f.getString("id")
            if (fid !in sent && socket.send(f.toString())) sent.add(fid)
        }
    }

    /** Abandons the current socket, if any. */
    private fun drop() {
        generation++
        ws?.cancel()
        ws = null
        connected = false
        connecting = false
        target = null
        remote.clear()
        if (status != Status.REJECTED) setStatus(Status.OFFLINE)
    }

    private fun scheduleRetry() {
        if (closed) return
        retry?.cancel(false)
        retry = exec.schedule({ run { connect() } }, backoff, TimeUnit.MILLISECONDS)
        backoff = minOf(backoff * 2, MAX_BACKOFF)
    }

    private fun setStatus(s: Status) {
        if (status == s) return
        status = s
        links.changed()
    }

    private companion object {
        const val MIN_BACKOFF = 1000L
        const val MAX_BACKOFF = 5 * 60 * 1000L
    }
}
