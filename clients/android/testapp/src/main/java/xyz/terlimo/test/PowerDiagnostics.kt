package xyz.terlimo.test

import android.util.Log

/**
 * §26.5 diagnostic iteration (temporary, secret-free): records which power/connect branch was
 * actually taken. Only counters, booleans and fixed phase/display tokens are allowed here —
 * never node ids, names, links or any credential material.
 */
internal object PowerDiagnostics {
    fun line(event: String, vararg fields: Pair<String, String?>) {
        val body = fields.joinToString(" ") { (key, value) -> "$key=${sanitize(value)}" }
        Log.i("WDTT/Power", "powerstage:$event $body")
    }

    fun flag(event: String, vararg fields: Pair<String, Boolean>) {
        line(event, *fields.map { (k, v) -> k to if (v) "1" else "0" }.toTypedArray())
    }

    private fun sanitize(value: String?): String =
        (value ?: "-").replace(Regex("[^A-Za-z0-9_.:|-]"), "").take(48)
}
