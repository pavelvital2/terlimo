package xyz.terlimo.test

import android.content.ContentResolver
import android.net.Uri
import java.io.ByteArrayOutputStream
import java.io.InputStream
import java.nio.ByteBuffer
import java.nio.charset.CodingErrorAction

object SubscriptionTextFileReader {
    private const val MAX_BYTES = 65_536

    fun read(resolver: ContentResolver, uri: Uri): String = resolver.openInputStream(uri).use { input ->
        require(input != null) { "IMPORT_FILE_INVALID" }
        readBounded(input)
    }

    internal fun readBounded(input: InputStream): String {
        val output = ByteArrayOutputStream()
        val buffer = ByteArray(4096)
        var total = 0
        while (true) {
            val read = input.read(buffer)
            if (read < 0) break
            total += read
            require(total <= MAX_BYTES) { "IMPORT_FILE_OVERSIZE" }
            output.write(buffer, 0, read)
        }
        val bytes = output.toByteArray()
        return try {
            runCatching {
                Charsets.UTF_8.newDecoder()
                    .onMalformedInput(CodingErrorAction.REPORT)
                    .onUnmappableCharacter(CodingErrorAction.REPORT)
                    .decode(ByteBuffer.wrap(bytes)).toString()
            }.getOrElse { throw IllegalArgumentException("IMPORT_FILE_INVALID") }
        } finally {
            bytes.fill(0)
        }
    }
}
